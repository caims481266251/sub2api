package vmruntime

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
)

type allocationDriver struct {
	fakeDriver
	bridge, guest  string
	conflicts      []string
	conflictError  error
	removedNetwork *NetworkLease
}

func (d *allocationDriver) NetworkPools() (string, string) { return d.bridge, d.guest }
func (d *allocationDriver) NetworkConflicts(context.Context) ([]string, error) {
	return d.conflicts, d.conflictError
}
func (d *allocationDriver) Remove(ctx context.Context, s State) error {
	if s.Network != nil {
		lease := *s.Network
		d.removedNetwork = &lease
	}
	return d.fakeDriver.Remove(ctx, s)
}
func allocationState(account int64) *State {
	return &State{AccountID: account, InstanceID: uuid.NewString()}
}

func TestNetworkAllocatorPersistentUniqueAndExternalConflict(t *testing.T) {
	driver := &allocationDriver{conflicts: []string{"10.240.0.0/28", "10.241.0.16/28"}}
	h, root := openTest(t, driver)
	a := allocationState(1)
	if e := h.reserveNetwork(context.Background(), a); e != nil {
		t.Fatal(e)
	}
	if a.Network.Subnet != "10.240.0.32/28" || a.Network.GuestSubnet != "10.241.0.32/28" || a.Network.Address != "10.240.0.34" || a.Network.GuestAddress != "10.241.0.36" {
		t.Fatalf("wrong allocation %+v", a.Network)
	}
	original := *a.Network
	if e := h.Close(); e != nil {
		t.Fatal(e)
	}
	h2, e := OpenHelper(root, driver)
	if e != nil {
		t.Fatal(e)
	}
	defer h2.Close()
	a.Network = nil // Simulate crash between reservation and manifest persistence.
	if e = h2.reserveNetwork(context.Background(), a); e != nil || *a.Network != original {
		t.Fatalf("restart lost lease: %v %+v", e, a.Network)
	}
	b := allocationState(2)
	if e = h2.reserveNetwork(context.Background(), b); e != nil {
		t.Fatal(e)
	}
	if b.Network.Subnet == a.Network.Subnet || b.Network.GuestSubnet == a.Network.GuestSubnet {
		t.Fatal("duplicate network")
	}
}
func TestNetworkAllocatorConcurrentReservationsAreUnique(t *testing.T) {
	h, _ := openTest(t, &allocationDriver{})
	var wg sync.WaitGroup
	states := make([]*State, 30)
	errs := make(chan error, 30)
	for i := range states {
		states[i] = allocationState(int64(i + 1))
		wg.Add(1)
		go func(s *State) { defer wg.Done(); errs <- h.reserveNetwork(context.Background(), s) }(states[i])
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	used := map[string]bool{}
	for _, s := range states {
		for _, subnet := range []string{s.Network.Subnet, s.Network.GuestSubnet} {
			if used[subnet] {
				t.Fatal("overlapping concurrent lease")
			}
			used[subnet] = true
		}
	}
}
func TestNetworkAllocatorExhaustionAndDeletionConfirmation(t *testing.T) {
	driver := &allocationDriver{bridge: "10.240.0.0/28", guest: "10.241.0.0/28"}
	h, _ := openTest(t, driver)
	s, e := h.Do(context.Background(), prepareRequest())
	if e != nil {
		t.Fatal(e)
	}
	reserved := *s.Network
	next := prepareRequest()
	next.AccountID++
	if _, e = h.Do(context.Background(), next); errorCode(e) != "network_pool_exhausted" {
		t.Fatalf("want exhausted: %v", e)
	}
	driver.removeError = fault("docker_operation_unknown")
	remove := Request{Version: 1, Operation: "remove", AccountID: s.AccountID, InstanceID: s.InstanceID, Generation: 2}
	s, e = h.Do(context.Background(), remove)
	if e == nil || s.Network.Released || *s.Network != reserved {
		t.Fatal("uncertain remove released lease")
	}
	if _, e = h.Do(context.Background(), next); errorCode(e) != "network_pool_exhausted" {
		t.Fatalf("failed removal freed lease: %v", e)
	}
	driver.removeError = nil
	s, e = h.Do(context.Background(), remove)
	if e != nil || !s.Network.Released {
		t.Fatalf("confirmed deletion did not release: %v", e)
	}
	n, e := h.Do(context.Background(), next)
	if e != nil {
		t.Fatal(e)
	}
	if n.Network.Subnet != reserved.Subnet || n.InstanceID == s.InstanceID {
		t.Fatal("freed subnet was not reusable by another instance")
	}
}
func TestNetworkAllocatorFailedCreateRetainsReservation(t *testing.T) {
	driver := &allocationDriver{bridge: "10.240.0.0/28", guest: "10.241.0.0/28"}
	driver.ensureError = fault("docker_operation_unknown")
	h, _ := openTest(t, driver)
	s, e := h.Do(context.Background(), prepareRequest())
	if e == nil || s.Network == nil {
		t.Fatal("unknown create did not retain lease")
	}
	lease := *s.Network
	driver.ensureError = nil
	retry := prepareRequest()
	retry.Generation = 2
	s, e = h.Do(context.Background(), retry)
	if e != nil || *s.Network != lease {
		t.Fatalf("retry changed lease: %v", e)
	}
}
func TestNetworkAllocatorLegacyManifestNotMigrated(t *testing.T) {
	driver := &allocationDriver{}
	h, root := openTest(t, driver)
	s, e := h.Do(context.Background(), prepareRequest())
	if e != nil {
		t.Fatal(e)
	}
	s.Network = nil
	if e = h.save(filepath.Join(root, s.InstanceID), s); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(root, "network-leases.json")); e != nil {
		t.Fatal(e)
	}
	req := prepareRequest()
	req.Generation = 2
	s, e = h.Do(context.Background(), req)
	if e != nil || s.Network != nil {
		t.Fatalf("legacy running instance migrated: %v", e)
	}
	if _, e = os.Stat(filepath.Join(root, "network-leases.json")); !os.IsNotExist(e) {
		t.Fatal("legacy instance unexpectedly reserved network")
	}
}
func TestNetworkAllocatorRejectsInvalidPoolsInventoryAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		bridge, guest string
		conflicts     []string
		want          string
	}{
		{"10.240.0.0/16", "10.240.0.0/16", nil, "network_pool_conflict"},
		{"8.8.0.0/16", "10.241.0.0/16", nil, "network_pool_invalid"},
		{"10.240.0.0/28", "10.241.0.0/28", []string{"invalid"}, "network_inventory_invalid"},
		{"10.240.0.0/28", "10.241.0.0/28", []string{"10.241.0.0/16"}, "network_pool_exhausted"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			h, _ := openTest(t, &allocationDriver{bridge: tc.bridge, guest: tc.guest, conflicts: tc.conflicts})
			if e := h.reserveNetwork(context.Background(), allocationState(1)); errorCode(e) != tc.want {
				t.Fatalf("%v", e)
			}
		})
	}
	h, _ := openTest(t, &allocationDriver{})
	s := allocationState(1)
	if e := h.reserveNetwork(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	s.AccountID = 2
	if e := h.reserveNetwork(context.Background(), s); errorCode(e) != "network_lease_conflict" {
		t.Fatal("ownership conflict accepted")
	}
}

func TestNetworkAllocatorRemovalRecoversReservationBeforeManifestWrite(t *testing.T) {
	driver := &allocationDriver{}
	h, _ := openTest(t, driver)
	_, s, e := h.resolve(prepareRequest())
	if e != nil {
		t.Fatal(e)
	}
	if e = h.reserveNetwork(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	lease := *s.Network // Ledger is durable; the manifest still has no network.
	removed, e := h.Do(context.Background(), Request{Version: 1, Operation: "remove", AccountID: s.AccountID, InstanceID: s.InstanceID, Generation: 2})
	if e != nil || removed.Network == nil || !removed.Network.Released || driver.removedNetwork == nil || *driver.removedNetwork != lease {
		t.Fatalf("remove lost persisted reservation: %v", e)
	}
	ledger, e := h.readNetworkLedger()
	if e != nil || len(ledger.Reservations) != 0 {
		t.Fatalf("orphan allocation after remove: %v", e)
	}
}

func TestNetworkAllocatorReleaseCrashIsIdempotent(t *testing.T) {
	h, _ := openTest(t, &allocationDriver{})
	s := allocationState(1)
	if e := h.reserveNetwork(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	staleManifest := *s
	if e := h.releaseNetwork(s); e != nil {
		t.Fatal(e)
	}
	// The manifest still carries the old unreleased value after the journal
	// removal. Retrying external removal and release must remain safe.
	if e := h.recoverNetwork(&staleManifest); e != nil {
		t.Fatal(e)
	}
	if e := h.releaseNetwork(&staleManifest); e != nil || !staleManifest.Network.Released {
		t.Fatalf("release retry failed: %v", e)
	}
}

func TestNetworkAllocatorCorruptLedgerFailsClosed(t *testing.T) {
	h, root := openTest(t, &allocationDriver{})
	s := allocationState(1)
	if e := h.reserveNetwork(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	ledger, e := h.readNetworkLedger()
	if e != nil {
		t.Fatal(e)
	}
	original := ledger.Reservations[s.InstanceID]
	corrupt := original
	corrupt.Lease.GuestAddress = corrupt.Lease.GuestGateway
	ledger.Reservations[s.InstanceID] = corrupt
	if e = atomicJSON(filepath.Join(root, "network-leases.json"), ledger); e != nil {
		t.Fatal(e)
	}
	if e = h.reserveNetwork(context.Background(), allocationState(2)); errorCode(e) != "network_lease_invalid" {
		t.Fatalf("corrupt address accepted: %v", e)
	}
	ledger.Reservations[s.InstanceID] = original
	other := uuid.NewString()
	duplicate := original
	duplicate.AccountID++
	duplicate.Lease.NetworkName = "sub2-vm-net-" + other
	ledger.Reservations[other] = duplicate
	if e = atomicJSON(filepath.Join(root, "network-leases.json"), ledger); e != nil {
		t.Fatal(e)
	}
	if e = h.reserveNetwork(context.Background(), allocationState(3)); errorCode(e) != "network_lease_conflict" {
		t.Fatalf("overlapping ledger accepted: %v", e)
	}
}
