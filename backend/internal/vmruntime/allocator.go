package vmruntime

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

const networkLeaseVersion = 1
const networkLeaseBits = 28

type networkReservation struct {
	AccountID int64        `json:"account_id"`
	Lease     NetworkLease `json:"lease"`
}
type networkLedger struct {
	Version      int                           `json:"version"`
	Reservations map[string]networkReservation `json:"reservations"`
}

func networkPool(raw string) (netip.Prefix, error) {
	p, e := netip.ParsePrefix(raw)
	if e != nil || !p.Addr().Is4() || !p.Addr().IsPrivate() || p != p.Masked() || p.Bits() < 16 || p.Bits() > networkLeaseBits {
		return netip.Prefix{}, fault("network_pool_invalid")
	}
	return p, nil
}
func networkAddress(base netip.Addr, offset uint32) string {
	b := base.As4()
	binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(b[:])+offset)
	return netip.AddrFrom4(b).String()
}
func leaseAt(instance string, bridge, guest netip.Prefix, slot uint32) NetworkLease {
	offset := slot << uint(32-networkLeaseBits)
	host := netip.MustParseAddr(networkAddress(bridge.Addr(), offset))
	vm := netip.MustParseAddr(networkAddress(guest.Addr(), offset))
	return NetworkLease{NetworkName: "sub2-vm-net-" + instance, Subnet: netip.PrefixFrom(host, networkLeaseBits).String(), Gateway: networkAddress(host, 1), Address: networkAddress(host, 2), GuestSubnet: netip.PrefixFrom(vm, networkLeaseBits).String(), GuestGateway: networkAddress(vm, 2), GuestDNS: networkAddress(vm, 3), GuestAddress: networkAddress(vm, 4)}
}
func (h *Helper) readNetworkLedger() (*networkLedger, error) {
	path := filepath.Join(h.root, "network-leases.json")
	if e := rejectSymlink(path); e != nil {
		return nil, e
	}
	raw, e := os.ReadFile(path)
	if errors.Is(e, os.ErrNotExist) {
		return &networkLedger{Version: networkLeaseVersion, Reservations: map[string]networkReservation{}}, nil
	}
	if e != nil {
		return nil, fault("network_lease_unreadable")
	}
	var ledger networkLedger
	if json.Unmarshal(raw, &ledger) != nil || ledger.Version != networkLeaseVersion || ledger.Reservations == nil {
		return nil, fault("network_lease_invalid")
	}
	used := []netip.Prefix{}
	for instance, reservation := range ledger.Reservations {
		lease := reservation.Lease
		parsed, parseErr := uuid.Parse(instance)
		if parseErr != nil || parsed.String() != instance || reservation.AccountID <= 0 || lease.NetworkName != "sub2-vm-net-"+instance || lease.Released {
			return nil, fault("network_lease_invalid")
		}
		for _, raw := range []string{lease.Subnet, lease.GuestSubnet} {
			p, e := networkPool(raw)
			if e != nil || p.Bits() != networkLeaseBits {
				return nil, fault("network_lease_invalid")
			}
			for _, old := range used {
				if p.Overlaps(old) {
					return nil, fault("network_lease_conflict")
				}
			}
			used = append(used, p)
		}
		host := netip.MustParsePrefix(lease.Subnet)
		guest := netip.MustParsePrefix(lease.GuestSubnet)
		if leaseAt(instance, host, guest, 0) != lease {
			return nil, fault("network_lease_invalid")
		}
	}
	return &ledger, nil
}

// reserveNetwork commits before any external create. An unsuccessful create
// retains its reservation because a daemon timeout does not prove absence.
func (h *Helper) reserveNetwork(ctx context.Context, s *State) error {
	h.networkMu.Lock()
	defer h.networkMu.Unlock()
	ledger, e := h.readNetworkLedger()
	if e != nil {
		return e
	}
	if existing, ok := ledger.Reservations[s.InstanceID]; ok {
		if existing.AccountID != s.AccountID || (s.Network != nil && *s.Network != existing.Lease) {
			return fault("network_lease_conflict")
		}
		lease := existing.Lease
		s.Network = &lease
		return nil
	}
	if s.Network != nil {
		return fault("network_lease_missing")
	}
	bridgeRaw, guestRaw := "10.240.0.0/16", "10.241.0.0/16"
	if provider, ok := h.driver.(interface{ NetworkPools() (string, string) }); ok {
		b, g := provider.NetworkPools()
		if b != "" {
			bridgeRaw = b
		}
		if g != "" {
			guestRaw = g
		}
	}
	bridge, e := networkPool(bridgeRaw)
	if e != nil {
		return e
	}
	guest, e := networkPool(guestRaw)
	if e != nil {
		return e
	}
	if bridge.Bits() != guest.Bits() || bridge.Overlaps(guest) {
		return fault("network_pool_conflict")
	}
	blocked := []netip.Prefix{}
	for _, reservation := range ledger.Reservations {
		blocked = append(blocked, netip.MustParsePrefix(reservation.Lease.Subnet), netip.MustParsePrefix(reservation.Lease.GuestSubnet))
	}
	if provider, ok := h.driver.(interface {
		NetworkConflicts(context.Context) ([]string, error)
	}); ok {
		external, e := provider.NetworkConflicts(ctx)
		if e != nil {
			return e
		}
		for _, raw := range external {
			p, e := netip.ParsePrefix(raw)
			if e != nil {
				return fault("network_inventory_invalid")
			}
			if p.Addr().Is4() {
				blocked = append(blocked, p.Masked())
			}
		}
	}
	for slot := uint32(0); slot < uint32(1)<<uint(networkLeaseBits-bridge.Bits()); slot++ {
		lease := leaseAt(s.InstanceID, bridge, guest, slot)
		b := netip.MustParsePrefix(lease.Subnet)
		g := netip.MustParsePrefix(lease.GuestSubnet)
		available := true
		for _, old := range blocked {
			if b.Overlaps(old) || g.Overlaps(old) {
				available = false
				break
			}
		}
		if !available {
			continue
		}
		ledger.Reservations[s.InstanceID] = networkReservation{AccountID: s.AccountID, Lease: lease}
		if e := atomicJSON(filepath.Join(h.root, "network-leases.json"), ledger); e != nil {
			return e
		}
		s.Network = &lease
		return nil
	}
	return fault("network_pool_exhausted")
}

// recoverNetwork restores a reservation persisted before its manifest write.
// Removal must see that reservation even if no external create was attempted.
func (h *Helper) recoverNetwork(s *State) error {
	h.networkMu.Lock()
	defer h.networkMu.Unlock()
	ledger, e := h.readNetworkLedger()
	if e != nil {
		return e
	}
	if existing, ok := ledger.Reservations[s.InstanceID]; ok {
		if existing.AccountID != s.AccountID || (s.Network != nil && *s.Network != existing.Lease) {
			return fault("network_lease_conflict")
		}
		lease := existing.Lease
		s.Network = &lease
	}
	return nil
}

// Called only after Driver.Remove has confirmed container and network absence.
func (h *Helper) releaseNetwork(s *State) error {
	if s.Network == nil || s.Network.Released {
		return nil
	}
	h.networkMu.Lock()
	defer h.networkMu.Unlock()
	ledger, e := h.readNetworkLedger()
	if e != nil {
		return e
	}
	if existing, ok := ledger.Reservations[s.InstanceID]; ok {
		if existing.AccountID != s.AccountID || existing.Lease != *s.Network {
			return fault("network_lease_conflict")
		}
		delete(ledger.Reservations, s.InstanceID)
		if e := atomicJSON(filepath.Join(h.root, "network-leases.json"), ledger); e != nil {
			return e
		}
	}
	lease := *s.Network
	lease.Released = true
	s.Network = &lease
	return nil
}
