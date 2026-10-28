package vmruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerPreflightUsesNoVM(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "usr/local/bin/kin-codex-kernel")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("test-binary")
	if err := os.WriteFile(path, payload, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	f := &containerPreflightCommands{}
	d := &DockerDriver{Commands: f, Templates: map[string]Template{"docker": {Runtime: "container-v1", Image: "sha256:" + strings.Repeat("a", 64), PayloadDir: root, Artifacts: map[string]string{path: hex.EncodeToString(sum[:])}}}}
	s := State{Spec: Spec{Template: "docker", Platform: "codex"}}
	if err := d.Preflight(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	for _, args := range f.calls {
		if strings.Contains(strings.Join(args, " "), "kvm") {
			t.Fatal("Docker preflight invoked KVM", args)
		}
	}
	f.host = "aarch64"
	if errorCode(d.Preflight(context.Background(), s)) != "image_host_architecture_mismatch" {
		t.Fatal("emulated network-tool image accepted")
	}
	f.host = "amd64"
	if err := os.WriteFile(path, []byte("changed-binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if errorCode(d.Preflight(context.Background(), s)) != "artifact_digest_mismatch" {
		t.Fatal("unpinned payload accepted")
	}
}

type containerObservationCommands struct {
	container  containerInspect
	generation uint64
	network    networkInspect
}

func (f *containerObservationCommands) Run(_ context.Context, args ...string) ([]byte, error) {
	switch args[0] {
	case "network":
		if args[1] == "ls" {
			return []byte(f.network.Name), nil
		}
		return json.Marshal([]networkInspect{f.network})
	case "container":
		return []byte(f.container.ID), nil
	case "inspect":
		return json.Marshal([]containerInspect{f.container})
	case "exec":
		return json.Marshal(map[string]any{"generation": f.generation, "process": "1234", "credential_revision": "credential-1", "proxy_revision": "proxy-1", "kernel_ready": true, "configuration_applied": true})
	}
	return nil, fault("unexpected_command")
}

func TestContainerObservationFencesProcessAndRejectsOldVM(t *testing.T) {
	// Keep UDS under /tmp because macOS paths in t.TempDir exceed sockaddr_un.
	dir, err := os.MkdirTemp("/tmp", "sub2-container-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Mkdir(filepath.Join(dir, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "run", "kernel.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	image := "sha256:" + strings.Repeat("a", 64)
	fixture, container, template := isolationFixture(t, dir)
	nd, networkState, nf := networkDriverFixture()
	if _, err := nd.ensureNetwork(context.Background(), networkState); err != nil {
		t.Fatal(err)
	}
	fixture.Network = networkState.Network
	container.HostConfig.NetworkMode = fixture.Network.NetworkName
	container.NetworkSettings.Networks = map[string]containerNetworkEndpoint{fixture.Network.NetworkName: {IPAddress: fixture.Network.Address, NetworkID: nf.network.ID, MacAddress: fixture.Identity.MAC}}
	f := &containerObservationCommands{generation: 2, container: container, network: *nf.network}
	f.container.ID = "container-1"
	f.container.State.Running = true
	f.container.State.StartedAt = "2026-10-23T00:00:00Z"
	f.container.Config.Image = image
	f.container.Config.Labels = map[string]string{"sub2.vm.instance": "instance-1", "sub2.vm.account": "42", "sub2.vm.runtime": "container-v1", "sub2.vm.service-gid": "0"}
	f.network.Labels["sub2.vm.instance"] = "instance-1"
	d := &DockerDriver{Commands: f, Templates: map[string]Template{"docker": {Runtime: "container-v1", Image: image, PayloadDir: template.PayloadDir}}}
	s := State{AccountID: 42, InstanceID: "instance-1", Generation: 2, Spec: Spec{Template: "docker"}, Identity: fixture.Identity, Network: fixture.Network}
	first, err := d.Inspect(context.Background(), s, dir)
	if err != nil || !first.KernelReady || first.ActualAccelerator != "docker" {
		t.Fatalf("%+v %v", first, err)
	}
	f.container.State.StartedAt = "2026-10-23T00:01:00Z"
	second, err := d.Inspect(context.Background(), s, dir)
	if err != nil || second.GuestBootID == first.GuestBootID {
		t.Fatal("restart did not invalidate binding")
	}
	f.generation = 1
	stale, err := d.Inspect(context.Background(), s, dir)
	if err != nil || !stale.Healthy || stale.KernelReady {
		t.Fatal("configuration drift must block admission without inventing a process fault", err)
	}

	f.container.HostConfig.ReadonlyRootfs = false
	if _, err := d.Inspect(context.Background(), s, dir); errorCode(err) != "runtime_isolation_mismatch" {
		t.Fatal("unsafe container remained observable as ready", err)
	}
	if _, err := d.Ensure(context.Background(), s, dir); errorCode(err) != "runtime_isolation_mismatch" {
		t.Fatal("unsafe container adopted on restart", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "run", "instance.json")); !os.IsNotExist(err) {
		t.Fatal("projection was written before isolation validation", err)
	}
	f.container.HostConfig.ReadonlyRootfs = true
	f.container.Config.Labels["sub2.vm.runtime"] = "upstream-v1"
	old, err := d.Inspect(context.Background(), s, dir)
	if err != nil || old.KernelReady || old.Running {
		t.Fatal("old VM admitted as Docker", err)
	}
}

type containerPreflightCommands struct {
	calls [][]string
	host  string
}

func (f *containerPreflightCommands) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if args[0] == "info" {
		if f.host != "" {
			return []byte(f.host), nil
		}
		return []byte("amd64"), nil
	}
	if args[0] == "image" {
		return []byte(`{"Os":"linux","Architecture":"amd64"}`), nil
	}
	return nil, nil
}

func TestContainerApplyFailsPromptlyWhenExited(t *testing.T) {
	image := "sha256:" + strings.Repeat("a", 64)
	f := &containerObservationCommands{}
	f.container.ID = "container-1"
	f.container.Config.Image = image
	f.container.Config.Labels = map[string]string{"sub2.vm.instance": "instance-1", "sub2.vm.account": "42"}
	d := &DockerDriver{Commands: f, Templates: map[string]Template{"docker": {Runtime: "container-v1", Image: image}}}
	s := State{AccountID: 42, InstanceID: "instance-1", Spec: Spec{Template: "docker"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := d.Apply(ctx, s, t.TempDir()); errorCode(err) != "container_not_running" {
		t.Fatal("exited container should fail without waiting for startup timeout", err)
	}
}

func TestDockerCommandDoesNotInheritEmulatedPlatform(t *testing.T) {
	t.Setenv("DOCKER_DEFAULT_PLATFORM", "linux/amd64")
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' \"$DOCKER_DEFAULT_PLATFORM\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := (ExecDocker{Binary: path}).Run(context.Background(), "info")
	if err != nil || len(out) != 0 {
		t.Fatalf("platform override inherited: %q %v", out, err)
	}
}

func TestContainerCreateExplicitlyBoundsResidentKernelLogs(t *testing.T) {
	driver, state, commands := networkDriverFixture()
	identity, err := newIdentity()
	if err != nil {
		t.Fatal(err)
	}
	state.Identity = identity
	dir := t.TempDir()
	template := driver.Templates[state.Spec.Template]
	template.PayloadDir = filepath.Join(dir, "payload")
	driver.Templates[state.Spec.Template] = template
	id, err := driver.Ensure(context.Background(), state, dir)
	if err != nil || id != "container-id" {
		t.Fatalf("create: id=%q err=%v", id, err)
	}
	var runs [][]string
	for _, args := range commands.calls {
		if len(args) > 0 && args[0] == "run" {
			runs = append(runs, args)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("expected one account container create, got %d", len(runs))
	}
	args := runs[0]
	if args[len(args)-1] != template.Image {
		t.Fatal("logging flags must be Docker options before the pinned image")
	}
	driverCount := 0
	options := map[string]int{}
	for i, arg := range args[:len(args)-1] {
		if strings.HasPrefix(arg, "--log-driver=") {
			driverCount++
			if arg != "--log-driver=json-file" {
				t.Fatalf("unexpected logging driver %q", arg)
			}
		}
		if arg == "--log-opt" {
			options[args[i+1]]++
		}
	}
	if driverCount != 1 || len(options) != 2 || options["max-size=10m"] != 1 || options["max-file=3"] != 1 {
		t.Fatalf("account logs inherit an unbounded or conflicting daemon policy: drivers=%d options=%v", driverCount, options)
	}
}
