package vmruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

func (d *DockerDriver) NetworkPool() string { return d.NetworkCIDR }

// Enumerate the daemon's networks rather than assuming the default address pool
// is unused. Errors are not absence and must stop resource allocation.
func (d *DockerDriver) NetworkConflicts(ctx context.Context) ([]string, error) {
	out, err := d.run(ctx, "network", "ls", "--format", "{{.ID}}")
	if err != nil {
		return nil, fault("network_inventory_unavailable")
	}
	var subnets []string
	for _, id := range strings.Fields(string(out)) {
		raw, err := d.run(ctx, "network", "inspect", id)
		if err != nil {
			return nil, fault("network_inventory_unavailable")
		}
		var rows []networkInspect
		if json.Unmarshal(raw, &rows) != nil || len(rows) != 1 {
			return nil, fault("network_inventory_invalid")
		}
		for _, ipam := range rows[0].IPAM.Config {
			if ipam.Subnet != "" {
				subnets = append(subnets, ipam.Subnet)
			}
		}
	}
	return subnets, nil
}

type networkInspect struct {
	ID, Name, Driver     string
	Internal, EnableIPv6 bool
	Labels               map[string]string
	Options              map[string]string
	IPAM                 struct {
		Config []struct{ Subnet, Gateway string }
	}
	Containers map[string]json.RawMessage
}

func (d *DockerDriver) inspectNetwork(ctx context.Context, name string) (*networkInspect, error) {
	out, err := d.run(ctx, "network", "ls", "--filter", "name="+name, "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	found := false
	for _, n := range strings.Fields(string(out)) {
		if n == name {
			found = true
		}
	}
	if !found {
		return nil, nil
	}
	out, err = d.run(ctx, "network", "inspect", name)
	if err != nil {
		return nil, err
	}
	var rows []networkInspect
	if json.Unmarshal(out, &rows) != nil || len(rows) != 1 || rows[0].Name != name {
		return nil, fault("network_inspect_invalid")
	}
	return &rows[0], nil
}
func ownedNetwork(n *networkInspect, s State) bool {
	return n != nil && n.Labels["sub2.vm.instance"] == s.InstanceID && n.Labels["sub2.vm.account"] == fmt.Sprint(s.AccountID)
}
func networkMatches(n *networkInspect, s State) bool {
	l := s.Network
	if l == nil || l.Released || !ownedNetwork(n, s) || n.Driver != "bridge" || n.Internal || n.EnableIPv6 || n.Options["com.docker.network.bridge.enable_icc"] != "false" || len(n.IPAM.Config) != 1 {
		return false
	}
	return n.IPAM.Config[0].Subnet == l.Subnet && n.IPAM.Config[0].Gateway == l.Gateway
}
func (d *DockerDriver) ensureNetwork(ctx context.Context, s State) (bool, error) {
	if s.Network == nil {
		return false, nil
	}
	if s.Network.Released {
		return false, fault("network_lease_released")
	}
	n, err := d.inspectNetwork(ctx, s.Network.NetworkName)
	if err != nil {
		return false, err
	}
	if n != nil {
		if !networkMatches(n, s) {
			return false, fault("network_ownership_conflict")
		}
		return false, nil
	}
	l := s.Network
	// Only helper-generated leases reach here; validate before command arguments.
	prefix, e := netip.ParsePrefix(l.Subnet)
	addr, ae := netip.ParseAddr(l.Address)
	if e != nil || ae != nil || !prefix.Addr().Is4() || prefix.Bits() != 28 || !prefix.Contains(addr) {
		return false, fault("network_lease_invalid")
	}
	_, err = d.run(ctx, "network", "create", "--driver=bridge", "--subnet="+l.Subnet, "--gateway="+l.Gateway, "--opt", "com.docker.network.bridge.enable_icc=false", "--label", "sub2.vm.instance="+s.InstanceID, "--label", fmt.Sprintf("sub2.vm.account=%d", s.AccountID), l.NetworkName)
	if err != nil {
		return false, fault("network_create_unknown")
	}
	n, err = d.inspectNetwork(ctx, l.NetworkName)
	if err != nil {
		return true, err
	}
	if !networkMatches(n, s) {
		return true, fault("network_configuration_unconfirmed")
	}
	return true, nil
}
func (d *DockerDriver) checkContainerNetwork(ctx context.Context, s State, c *containerInspect) error {
	if s.Network == nil {
		return nil
	} // Existing accounts keep their legacy network until explicit migration.
	n, err := d.inspectNetwork(ctx, s.Network.NetworkName)
	if err != nil {
		return err
	}
	if !networkMatches(n, s) {
		return fault("network_configuration_unconfirmed")
	}
	if len(c.NetworkSettings.Networks) != 1 {
		return fault("runtime_network_mismatch")
	}
	endpoint, ok := c.NetworkSettings.Networks[s.Network.NetworkName]
	if !ok || endpoint.IPAddress != s.Network.Address || endpoint.NetworkID != n.ID {
		return fault("runtime_network_mismatch")
	}
	return nil
}

// Stopped endpoints may have no assigned IP yet. Check the persisted static
// address intent and exclusive network membership before starting any process.
func (d *DockerDriver) checkStoppedContainerNetwork(ctx context.Context, s State, c *containerInspect) error {
	if s.Network == nil {
		return nil
	}
	n, err := d.inspectNetwork(ctx, s.Network.NetworkName)
	if err != nil {
		return err
	}
	if !networkMatches(n, s) {
		return fault("network_configuration_unconfirmed")
	}
	if c.HostConfig.NetworkMode != s.Network.NetworkName || len(c.NetworkSettings.Networks) != 1 {
		return fault("runtime_network_mismatch")
	}
	endpoint, ok := c.NetworkSettings.Networks[s.Network.NetworkName]
	if !ok || endpoint.IPAMConfig == nil || endpoint.IPAMConfig.IPv4Address != s.Network.Address || (endpoint.NetworkID != "" && endpoint.NetworkID != n.ID) {
		return fault("runtime_network_mismatch")
	}
	return nil
}
func (d *DockerDriver) removeNetwork(ctx context.Context, s State) error {
	if s.Network == nil || s.Network.Released {
		return nil
	}
	n, err := d.inspectNetwork(ctx, s.Network.NetworkName)
	if err != nil {
		return err
	}
	if n == nil {
		return nil
	}
	if !ownedNetwork(n, s) {
		return fault("network_ownership_conflict")
	}
	if len(n.Containers) != 0 {
		return fault("network_still_in_use")
	}
	if _, err = d.run(ctx, "network", "rm", n.ID); err != nil {
		return fault("network_removal_unknown")
	}
	n, err = d.inspectNetwork(ctx, s.Network.NetworkName)
	if err != nil {
		return err
	}
	if n != nil {
		return fault("network_removal_unconfirmed")
	}
	return nil
}
