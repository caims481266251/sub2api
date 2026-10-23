package vmruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Opt-in daemon test: creates only uniquely labelled, disposable networks and
// one inert sleep container; it never opens provider connections or uses tokens.
func TestDockerLiveIsolatedAllocationAndRestart(t *testing.T) {
	if os.Getenv("SUB2_VM_NETWORK_LIVE_TEST") != "1" {
		t.Skip("explicit local Docker verification only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	d := &DockerDriver{}
	image, err := d.run(ctx, "image", "inspect", "--format", "{{.Id}}", "debian:trixie-slim")
	if err != nil {
		t.Fatal(err)
	}
	d.Templates = map[string]Template{"fixture": {Runtime: "container-v1", Image: strings.TrimSpace(string(image))}}
	h, err := OpenHelper(t.TempDir(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	a := State{AccountID: 900001, InstanceID: uuid.NewString(), Spec: Spec{Template: "fixture"}}
	b := State{AccountID: 900002, InstanceID: uuid.NewString(), Spec: Spec{Template: "fixture"}}

	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		for _, state := range []State{a, b} {
			if e := d.Stop(cleanup, state); e != nil {
				t.Error(e)
			}
			if e := d.Remove(cleanup, state); e != nil {
				t.Error(e)
			}
		}
	})
	for _, s := range []*State{&a, &b} {
		if err = h.reserveNetwork(ctx, s); err != nil {
			t.Fatal(err)
		}
		if _, err = d.ensureNetwork(ctx, *s); err != nil {
			t.Fatal(err)
		}
	}
	if a.Network.Subnet == b.Network.Subnet {
		t.Fatal("allocation collision")
	}
	args := []string{"run", "-d", "--pull=never", "--name", containerName(a), "--network=" + a.Network.NetworkName, "--ip=" + a.Network.Address, "--label", "sub2.vm.instance=" + a.InstanceID, "--label", "sub2.vm.account=900001", "--label", "sub2.vm.runtime=container-v1", "--label", "sub2.vm.service-gid=0", "--entrypoint=/bin/sleep", strings.TrimSpace(string(image)), "120"}
	if _, err = d.run(ctx, args...); err != nil {
		t.Fatal(err)
	}
	c, err := d.inspectContainer(ctx, containerName(a))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.checkContainerNetwork(ctx, a, c); err != nil {
		t.Fatal(err)
	}
	if errorCode(d.removeNetwork(ctx, a)) != "network_still_in_use" {
		t.Fatal("removed occupied network")
	}
	if err = d.Stop(ctx, a); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.Mkdir(filepath.Join(dir, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Ensure(ctx, a, dir); err != nil {
		t.Fatal("restart rejected", err)
	}
	c, err = d.inspectContainer(ctx, containerName(a))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.checkContainerNetwork(ctx, a, c); err != nil {
		t.Fatal(err)
	}
	if err = d.Stop(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err = d.run(ctx, "network", "connect", b.Network.NetworkName, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Ensure(ctx, a, dir); errorCode(err) != "runtime_network_mismatch" {
		t.Fatal("foreign attachment not rejected", err)
	}
	c, err = d.inspectContainer(ctx, containerName(a))
	if err != nil || c.State.Running {
		t.Fatal("network mismatch started a container", err)
	}
	if _, err = d.run(ctx, "network", "disconnect", b.Network.NetworkName, c.ID); err != nil {
		t.Fatal(err)
	}
	t.Log("two unique network leases; static IP preserved on restart; occupied network retained; foreign attachment rejected before start")
}
