package vmruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (d *DockerDriver) containerPreflight(ctx context.Context, s State) error {
	t, err := d.template(s)
	if err != nil {
		return err
	}
	if s.Spec.Platform != "codex" {
		return fault("container_platform_unsupported")
	}
	if !filepath.IsAbs(t.PayloadDir) {
		return fault("payload_path_invalid")
	}
	info, err := os.Lstat(t.PayloadDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fault("payload_path_invalid")
	}
	count := 0
	err = filepath.WalkDir(t.PayloadDir, func(path string, e os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fault("payload_unreadable")
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fault("payload_symlink_forbidden")
		}
		if e.IsDir() {
			return nil
		}
		digest, ok := t.Artifacts[path]
		if !ok || !e.Type().IsRegular() {
			return fault("unmanifested_payload")
		}
		count++
		return VerifyArtifact(path, digest)
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return fault("artifact_manifest_required")
	}
	kernel := filepath.Join(t.PayloadDir, "usr/local/bin/kin-codex-kernel")
	if _, ok := t.Artifacts[kernel]; !ok {
		return fault("artifact_missing")
	}
	raw, err := d.run(ctx, "image", "inspect", "--format", "{{json .}}", t.Image)
	if err != nil {
		return fault("pinned_image_missing")
	}
	var image struct{ Os, Architecture string }
	if json.Unmarshal(raw, &image) != nil || image.Os != "linux" || (image.Architecture != "amd64" && image.Architecture != "arm64") {
		return fault("image_platform_mismatch")
	}
	// Network tools must use the host ISA: emulated nft cannot open Netlink.
	host, err := d.run(ctx, "info", "--format", "{{.Architecture}}")
	if err != nil {
		return fault("docker_host_unavailable")
	}
	arch := strings.TrimSpace(string(host))
	if arch == "aarch64" {
		arch = "arm64"
	}
	if arch == "x86_64" {
		arch = "amd64"
	}
	if arch != image.Architecture {
		return fault("image_host_architecture_mismatch")
	}
	_, err = d.run(ctx, "run", "--rm", "--pull=never", "--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--entrypoint=/bin/true", t.Image)
	return err
}

func (d *DockerDriver) containerEnsure(ctx context.Context, s State, dir string) (string, error) {
	t, err := d.template(s)
	if err != nil {
		return "", err
	}
	c, err := d.inspectContainer(ctx, containerName(s))
	if err != nil {
		return "", err
	}
	if c != nil {
		if !owned(c, s) {
			return "", fault("runtime_ownership_conflict")
		}
		// Existing instances are never migrated implicitly. Only newly created
		// Docker instances can be adopted by this driver.
		if c.Config.Labels["sub2.vm.runtime"] != "container-v1" || c.Config.Image != t.Image {
			return "", fault("runtime_policy_conflict")
		}
	}
	// Inspect before touching any existing projection or starting a process.
	if c != nil {
		if err := checkContainerIsolation(s, c, dir, t); err != nil {
			return "", err
		}
	}
	if err := writeInstanceProjection(dir, s, d.ServiceGID); err != nil {
		return "", err
	}
	if c != nil {
		if c.Config.Labels["sub2.vm.service-gid"] != fmt.Sprint(d.ServiceGID) {
			return "", fault("runtime_policy_conflict")
		}
		if c.State.Running {
			if err := d.checkContainerNetwork(ctx, s, c); err != nil {
				return "", err
			}
			return c.ID, nil
		}
		if err := d.checkStoppedContainerNetwork(ctx, s, c); err != nil {
			return "", err
		}
		if _, err := d.run(ctx, "start", c.ID); err != nil {
			return "", err
		}
		return c.ID, nil
	}
	for _, sub := range []string{"home", "run", "credentials"} {
		if err := privateDir(filepath.Join(dir, sub)); err != nil {
			return "", err
		}
	}
	if _, err := d.ensureNetwork(ctx, s); err != nil {
		return "", err
	}
	if s.Network == nil {
		return "", fault("network_lease_missing")
	}
	network, err := d.inspectNetwork(ctx, s.Network.NetworkName)
	if err != nil {
		return "", err
	}
	if network == nil {
		return "", fault("network_configuration_unconfirmed")
	}
	if err := checkNetworkMembers(network, ""); err != nil {
		return "", err
	}
	machineID := filepath.Join(dir, "run", "machine-id")
	if err := atomicBytes(machineID, []byte(strings.ReplaceAll(s.Identity.MachineID, "-", "")+"\n")); err != nil {
		return "", err
	}
	if err := os.Chmod(machineID, 0644); err != nil {
		return "", err
	}
	// Docker makes its generated /etc/hosts read-only with --read-only. Supply
	// a root-owned file so bootstrap can pin the proxy hostname before dropping
	// privileges; the kernel cannot modify this file or its parent directory.
	hosts := filepath.Join(dir, "hosts")
	if err := atomicBytes(hosts, []byte("127.0.0.1 localhost\n")); err != nil {
		return "", err
	}
	if err := os.Chmod(hosts, 0644); err != nil {
		return "", err
	}
	args := []string{"run", "-d", "--pull=never", "--name", containerName(s), "--hostname", s.Identity.Hostname,
		// Do not inherit an unbounded daemon logging policy for resident kernels.
		"--log-driver=json-file", "--log-opt", "max-size=10m", "--log-opt", "max-file=3",
		"--network=" + s.Network.NetworkName, "--mac-address=" + s.Identity.MAC, "--ip=" + s.Network.Address, "--sysctl=net.ipv6.conf.all.disable_ipv6=1",
		"--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,size=32m", "--tmpfs", "/run:rw,nosuid,size=16m",
		"--cap-drop=ALL", "--cap-add=NET_ADMIN", "--cap-add=CHOWN", "--cap-add=SETUID", "--cap-add=SETGID", "--cap-add=DAC_OVERRIDE", "--cap-add=SETPCAP",
		"--security-opt=no-new-privileges", "--stop-timeout=40", "--pids-limit=128", "--memory", fmt.Sprintf("%dm", s.Spec.MemoryMiB),
		"--label", "sub2.vm.instance=" + s.InstanceID, "--label", fmt.Sprintf("sub2.vm.account=%d", s.AccountID),
		"--label", "sub2.vm.runtime=container-v1", "--label", fmt.Sprintf("sub2.vm.service-gid=%d", d.ServiceGID),
		"--env", fmt.Sprintf("SUB2_VM_SERVICE_GID=%d", d.ServiceGID), "--env", "HOME=/slot/home"}
	for _, sub := range []string{"home", "run", "credentials"} {
		mount := "type=bind,src=" + filepath.Join(dir, sub) + ",dst=/slot/" + sub
		if sub == "credentials" {
			mount += ",readonly"
		}
		args = append(args, "--mount", mount)
	}
	args = append(args, "--mount", "type=bind,src="+hosts+",dst=/etc/hosts", "--mount", "type=bind,src="+machineID+",dst=/etc/machine-id,readonly", "--mount", "type=bind,src="+t.PayloadDir+",dst=/opt/kin-payload,readonly", t.Image)
	raw, err := d.run(ctx, args...)
	if err != nil {
		return "", err
	} // Retain lease on an unknown create outcome.
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fault("docker_create_unknown")
	}
	return id, nil
}

func (d *DockerDriver) containerInspect(ctx context.Context, s State, dir string) (Observation, error) {
	c, err := d.inspectContainer(ctx, containerName(s))
	if err != nil {
		return Observation{}, err
	}
	if c == nil {
		return Observation{}, nil
	}
	if !owned(c, s) {
		return Observation{}, fault("runtime_ownership_conflict")
	}
	t, err := d.template(s)
	if err != nil {
		return Observation{}, err
	}
	// Only the configured container runtime and immutable image are admitted.
	if !c.State.Running || c.Config.Labels["sub2.vm.runtime"] != "container-v1" || c.Config.Image != t.Image {
		return Observation{}, nil
	}
	if c.Config.Labels["sub2.vm.service-gid"] != fmt.Sprint(d.ServiceGID) {
		return Observation{}, fault("runtime_policy_conflict")
	}
	if err := checkContainerIsolation(s, c, dir, t); err != nil {
		return Observation{}, err
	}
	if err := d.checkContainerNetwork(ctx, s, c); err != nil {
		return Observation{}, err
	}
	raw, err := d.run(ctx, "exec", containerName(s), "/usr/local/lib/sub2-container/observe")
	if err != nil {
		return Observation{}, err
	}
	var receipt struct {
		Generation           uint64 `json:"generation"`
		CredentialRevision   string `json:"credential_revision"`
		ProxyRevision        string `json:"proxy_revision"`
		Process              string `json:"process"`
		KernelReady          bool   `json:"kernel_ready"`
		ConfigurationApplied bool   `json:"configuration_applied"`
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.Process == "" || c.State.StartedAt == "" {
		return Observation{}, fault("runtime_observation_invalid")
	}
	socket := filepath.Join(dir, "run", "kernel.sock")
	st, err := os.Lstat(socket)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		return Observation{}, fault("kernel_socket_invalid")
	}
	if d.ServiceGID > 0 {
		if err := os.Chown(socket, 10001, d.ServiceGID); err != nil {
			return Observation{}, err
		}
		if err := os.Chmod(socket, 0660); err != nil {
			return Observation{}, err
		}
	}
	return Observation{Healthy: receipt.KernelReady, ActualAccelerator: "docker", Running: true, GuestBootID: c.ID + ":" + c.State.StartedAt + ":" + receipt.Process,
		CredentialRevision: receipt.CredentialRevision, ProxyRevision: receipt.ProxyRevision, KernelReady: receipt.KernelReady && receipt.ConfigurationApplied && receipt.Generation == s.Generation}, nil
}

func (d *DockerDriver) containerApply(ctx context.Context, s State, dir string) (Observation, error) {
	// Helper already stops and drains before changing a projection. Never restart
	// a healthy container here just because the application process restarted.
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		observation, err := d.containerInspect(ctx, s, dir)
		if err == nil && observation.KernelReady {
			return observation, nil
		}
		if err == nil && !observation.Running {
			return Observation{}, fault("container_not_running")
		}
		select {
		case <-ctx.Done():
			return Observation{}, ctx.Err()
		case <-deadline.C:
			return Observation{}, fault("container_start_timeout")
		case <-time.After(time.Second):
		}
	}
}
