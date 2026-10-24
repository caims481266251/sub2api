package vmruntime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Opt-in daemon test: creates only uniquely labelled, disposable networks and
// two inert sleep containers; it never opens provider connections or uses tokens.
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
	a.Identity.MAC = "02:00:00:90:00:01"
	args := []string{"run", "-d", "--init", "--pull=never", "--name", containerName(a), "--network=" + a.Network.NetworkName, "--ip=" + a.Network.Address, "--mac-address=" + a.Identity.MAC, "--label", "sub2.vm.instance=" + a.InstanceID, "--label", "sub2.vm.account=900001", "--label", "sub2.vm.runtime=container-v1", "--label", "sub2.vm.service-gid=0", "--entrypoint=/bin/sleep", strings.TrimSpace(string(image)), "120"}
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

	// A second account has an independent namespace. Attaching it to A's
	// network must invalidate A even though A still has only its own endpoint.
	if _, err = d.run(ctx, "run", "-d", "--init", "--pull=never", "--name", containerName(b),
		"--network="+b.Network.NetworkName, "--ip="+b.Network.Address,
		"--label", "sub2.vm.instance="+b.InstanceID, "--label", "sub2.vm.account=900002",
		"--entrypoint=/bin/sleep", strings.TrimSpace(string(image)), "120"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.run(ctx, "exec", containerName(a), "sh", "-c", "echo account-a > /tmp/account-private"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.run(ctx, "exec", containerName(b), "test", "!", "-e", "/tmp/account-private"); err != nil {
		t.Fatal("private filesystem leaked", err)
	}
	if _, err = d.run(ctx, "network", "connect", a.Network.NetworkName, containerName(b)); err != nil {
		t.Fatal(err)
	}
	foreignErr := d.checkContainerNetwork(ctx, a, c)
	if _, err = d.run(ctx, "network", "disconnect", a.Network.NetworkName, containerName(b)); err != nil {
		t.Fatal(err)
	}
	if errorCode(foreignErr) != "runtime_network_foreign_member" {
		t.Fatal("foreign network member accepted", foreignErr)
	}
	if errorCode(d.removeNetwork(ctx, a)) != "network_still_in_use" {
		t.Fatal("removed occupied network")
	}
	if err = d.Stop(ctx, a); err != nil {
		t.Fatal(err)
	}
	c, err = d.inspectContainer(ctx, containerName(a))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.checkStoppedContainerNetwork(ctx, a, c); err != nil {
		t.Fatal("restart rejected", err)
	}
	if _, err = d.run(ctx, "start", c.ID); err != nil {
		t.Fatal(err)
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
	c, err = d.inspectContainer(ctx, containerName(a))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.checkStoppedContainerNetwork(ctx, a, c); errorCode(err) != "runtime_network_mismatch" {
		t.Fatal("foreign attachment not rejected", err)
	}
	c, err = d.inspectContainer(ctx, containerName(a))
	if err != nil || c.State.Running {
		t.Fatal("network mismatch started a container", err)
	}
	if _, err = d.run(ctx, "network", "disconnect", b.Network.NetworkName, c.ID); err != nil {
		t.Fatal(err)
	}
	other, err := d.inspectContainer(ctx, containerName(b))
	if err != nil || !other.State.Running {
		t.Fatal("other account interrupted", err)
	}
	t.Log("two unique networks and private filesystems; static IP preserved; foreign network member and attachment rejected; other account unaffected")
}
