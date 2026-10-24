package vmruntime

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

type networkCommands struct {
	network                               *networkInspect
	calls                                 [][]string
	createErr, runErr, listErr, removeErr bool
	containerPresent                      bool
	state                                 State
}

func (f *networkCommands) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if args[0] == "network" {
		switch args[1] {
		case "ls":
			if f.listErr {
				return nil, fault("fixture_daemon_down")
			}
			if f.network == nil {
				return nil, nil
			}
			return []byte(f.network.Name), nil
		case "inspect":
			if f.network == nil {
				return nil, fault("fixture_absent")
			}
			return json.Marshal([]*networkInspect{f.network})
		case "create":
			if f.createErr {
				return nil, fault("fixture_create_failed")
			}
			l := f.state.Network
			n := &networkInspect{ID: "network-id", Name: l.NetworkName, Driver: "bridge", Labels: map[string]string{"sub2.vm.instance": f.state.InstanceID, "sub2.vm.account": "42"}, Options: map[string]string{"com.docker.network.bridge.enable_icc": "false"}, Containers: map[string]json.RawMessage{}}
			n.IPAM.Config = append(n.IPAM.Config, struct{ Subnet, Gateway string }{l.Subnet, l.Gateway})
			f.network = n
			return []byte(n.ID), nil
		case "rm":
			if f.removeErr {
				return nil, fault("fixture_rm_failed")
			}
			f.network = nil
			return nil, nil
		}
	}
	if args[0] == "container" {
		if f.containerPresent {
			return []byte("container-id"), nil
		}
		return nil, nil
	}
	if args[0] == "inspect" {
		c := containerInspect{ID: "container-id"}
		c.Config.Labels = map[string]string{"sub2.vm.instance": f.state.InstanceID, "sub2.vm.account": "42"}
		return json.Marshal([]containerInspect{c})
	}
	if args[0] == "run" {
		if f.runErr {
			return nil, fault("fixture_run_unknown")
		}
		return []byte("container-id"), nil
	}
	return nil, fault("unexpected_test_command")
}
func networkDriverFixture() (*DockerDriver, State, *networkCommands) {
	s := State{AccountID: 42, InstanceID: "test-instance"}
	l := leaseAt(s.InstanceID, netip.MustParsePrefix("10.240.0.0/16"), 0)
	s.Network = &l
	f := &networkCommands{state: s}
	return &DockerDriver{Commands: f, Templates: map[string]Template{"": {Runtime: "container-v1", Image: "sha256:" + strings.Repeat("a", 64)}}}, s, f
}
func TestDockerNetworkLifecycleAndOwnership(t *testing.T) {
	d, s, f := networkDriverFixture()
	ctx := context.Background()
	created, err := d.ensureNetwork(ctx, s)
	if err != nil || !created {
		t.Fatalf("create %v %v", created, err)
	}
	created, err = d.ensureNetwork(ctx, s)
	if err != nil || created {
		t.Fatalf("reuse %v %v", created, err)
	}
	f.network.Labels["sub2.vm.account"] = "99"
	if _, err = d.ensureNetwork(ctx, s); errorCode(err) != "network_ownership_conflict" {
		t.Fatal(err)
	}
	if err = d.removeNetwork(ctx, s); errorCode(err) != "network_ownership_conflict" {
		t.Fatal(err)
	}
	f.network.Labels["sub2.vm.account"] = "42"
	f.network.Containers["other"] = json.RawMessage(`{}`)
	if err = d.removeNetwork(ctx, s); errorCode(err) != "network_still_in_use" {
		t.Fatal(err)
	}
	delete(f.network.Containers, "other")
	f.removeErr = true
	if err = d.removeNetwork(ctx, s); errorCode(err) != "network_removal_unknown" {
		t.Fatal(err)
	}
	f.removeErr = false
	if err = d.removeNetwork(ctx, s); err != nil || f.network != nil {
		t.Fatalf("remove %v", err)
	}
}
func TestDockerNetworkCreateFailureNeverStartsGuest(t *testing.T) {
	d, s, f := networkDriverFixture()
	f.createErr = true
	_, err := d.Ensure(context.Background(), s, t.TempDir())
	if errorCode(err) != "network_create_unknown" {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c[0] == "run" {
			t.Fatal("guest started after network failure")
		}
	}
}
func TestDockerFailedCreateRetainsNetworkForReconciliation(t *testing.T) {
	d, s, f := networkDriverFixture()
	f.runErr = true
	if _, err := d.Ensure(context.Background(), s, t.TempDir()); err == nil {
		t.Fatal("expected create failure")
	}
	if f.network == nil {
		t.Fatal("network was prematurely released")
	}
}
func TestDockerNetworkUnavailableInventoryNeverCreates(t *testing.T) {
	d, s, f := networkDriverFixture()
	f.listErr = true
	if _, err := d.ensureNetwork(context.Background(), s); err == nil {
		t.Fatal("missing inventory accepted")
	}
	for _, c := range f.calls {
		if strings.Join(c[:2], " ") == "network create" {
			t.Fatal("create despite unavailable inventory")
		}
	}
}
func TestDockerNetworkContainerAddressAndAttachmentVerified(t *testing.T) {
	d, s, f := networkDriverFixture()
	ctx := context.Background()
	_, err := d.ensureNetwork(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	c := &containerInspect{}
	c.NetworkSettings.Networks = map[string]containerNetworkEndpoint{s.Network.NetworkName: {IPAddress: s.Network.Address, NetworkID: f.network.ID}}
	if err = d.checkContainerNetwork(ctx, s, c); err != nil {
		t.Fatal(err)
	}
	c.NetworkSettings.Networks["bridge"] = containerNetworkEndpoint{IPAddress: "172.17.0.2", NetworkID: "extra"}
	if errorCode(d.checkContainerNetwork(ctx, s, c)) != "runtime_network_mismatch" {
		t.Fatal("extra attachment accepted")
	}
	delete(c.NetworkSettings.Networks, "bridge")
	c.NetworkSettings.Networks[s.Network.NetworkName] = containerNetworkEndpoint{IPAddress: "10.240.0.3", NetworkID: f.network.ID}
	if errorCode(d.checkContainerNetwork(ctx, s, c)) != "runtime_network_mismatch" {
		t.Fatal("wrong IP accepted")
	}
}

func TestDockerStoppedNetworkValidatesIntentBeforeStart(t *testing.T) {
	d, s, f := networkDriverFixture()
	ctx := context.Background()
	_, err := d.ensureNetwork(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	c := &containerInspect{}
	c.HostConfig.NetworkMode = s.Network.NetworkName
	c.NetworkSettings.Networks = map[string]containerNetworkEndpoint{s.Network.NetworkName: {NetworkID: f.network.ID, IPAMConfig: &struct{ IPv4Address string }{s.Network.Address}}}
	if err = d.checkStoppedContainerNetwork(ctx, s, c); err != nil {
		t.Fatal(err)
	}
	c.HostConfig.NetworkMode = "bridge"
	if errorCode(d.checkStoppedContainerNetwork(ctx, s, c)) != "runtime_network_mismatch" {
		t.Fatal("foreign network intent accepted")
	}
	c.HostConfig.NetworkMode = s.Network.NetworkName
	c.NetworkSettings.Networks[s.Network.NetworkName] = containerNetworkEndpoint{}
	if errorCode(d.checkStoppedContainerNetwork(ctx, s, c)) != "runtime_network_mismatch" {
		t.Fatal("missing static address accepted")
	}
}

func TestNetworkRejectsForeignMembersAndMAC(t *testing.T) {
	d, s, f := networkDriverFixture()
	s.Identity.MAC = "02:11:22:33:44:55"
	if _, err := d.ensureNetwork(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	c := &containerInspect{ID: "own-container"}
	c.NetworkSettings.Networks = map[string]containerNetworkEndpoint{s.Network.NetworkName: {IPAddress: s.Network.Address, NetworkID: f.network.ID, MacAddress: s.Identity.MAC}}
	f.network.Containers[c.ID] = json.RawMessage(`{}`)
	if err := d.checkContainerNetwork(context.Background(), s, c); err != nil {
		t.Fatal(err)
	}
	f.network.Containers["foreign"] = json.RawMessage(`{}`)
	if err := d.checkContainerNetwork(context.Background(), s, c); errorCode(err) != "runtime_network_foreign_member" {
		t.Fatal(err)
	}
	delete(f.network.Containers, "foreign")
	ep := c.NetworkSettings.Networks[s.Network.NetworkName]
	ep.MacAddress = "02:00:00:00:00:01"
	c.NetworkSettings.Networks[s.Network.NetworkName] = ep
	if err := d.checkContainerNetwork(context.Background(), s, c); errorCode(err) != "runtime_network_mismatch" {
		t.Fatal(err)
	}
}
