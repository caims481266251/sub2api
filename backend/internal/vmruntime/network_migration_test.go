package vmruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type migrationDriver struct {
	fakeDriver
	stopError    error
	beforeRemove func(State)
}

func (d *migrationDriver) Stop(ctx context.Context, s State) error {
	if d.stopError != nil {
		return d.stopError
	}
	return d.fakeDriver.Stop(ctx, s)
}
func (d *migrationDriver) Remove(ctx context.Context, s State) error {
	if d.beforeRemove != nil {
		d.beforeRemove(s)
	}
	return d.fakeDriver.Remove(ctx, s)
}
func legacyMigrationFixture(t *testing.T) (*Helper, string, *State, *migrationDriver) {
	t.Helper()
	d := &migrationDriver{}
	h, root := openTest(t, d)
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
	return h, root, s, d
}

func TestNetworkMigrationPreservesIdentityDataAndReplaysWithoutRemoval(t *testing.T) {
	h, root, old, d := legacyMigrationFixture(t)
	dir := filepath.Join(root, old.InstanceID)
	if e := os.MkdirAll(filepath.Join(dir, "kvm"), 0700); e != nil {
		t.Fatal(e)
	}
	disk := filepath.Join(dir, "kvm", "guest-fixture.disk")
	if e := os.WriteFile(disk, []byte("retained-disk-content"), 0600); e != nil {
		t.Fatal(e)
	}
	d.beforeRemove = func(s State) {
		if s.Network != nil || s.Generation != 2 || s.Ready {
			t.Fatal("migration did not fence legacy instance before removal")
		}
		stored, e := h.read(dir)
		if e != nil || stored.Generation != 2 || stored.Ready || stored.Network != nil {
			t.Fatal("migration fence was not durable before removal")
		}
		if _, e = os.Stat(filepath.Join(root, "network-leases.json")); !os.IsNotExist(e) {
			t.Fatal("migration allocated network before legacy removal confirmation")
		}
	}
	req := prepareRequest()
	req.Generation, req.MigrateNetwork = 2, true
	s, e := h.Do(context.Background(), req)
	if e != nil || !s.Ready || s.Network == nil {
		t.Fatalf("migration failed: %v", e)
	}
	if s.InstanceID != old.InstanceID || s.Identity != old.Identity || *s.ClientIdentity != *old.ClientIdentity || !s.DataRetained || s.DeleteRequested {
		t.Fatal("migration changed instance identity or data retention")
	}
	if raw, e := os.ReadFile(disk); e != nil || string(raw) != "retained-disk-content" {
		t.Fatal("migration did not preserve disk")
	}
	lease := *s.Network
	removes := d.removeCount
	ensures := d.ensureCount
	s, e = h.Do(context.Background(), req)
	if e != nil || *s.Network != lease || d.removeCount != removes || d.ensureCount != ensures {
		t.Fatalf("replayed migration recreated existing leased instance: %v", e)
	}
}

func TestNetworkMigrationFailureDoesNotAllocateOrClearLegacyBinding(t *testing.T) {
	for _, failStop := range []bool{true, false} {
		h, root, old, d := legacyMigrationFixture(t)
		if failStop {
			d.stopError = fault("runtime_stop_unknown")
		} else {
			d.removeError = fault("runtime_removal_unknown")
		}
		req := prepareRequest()
		req.Generation, req.MigrateNetwork = 2, true
		s, e := h.Do(context.Background(), req)
		if e == nil || s.Ready || s.Network != nil || s.ContainerID != old.ContainerID || s.Identity != old.Identity {
			t.Fatalf("failed migration changed legacy binding: %v", e)
		}
		if _, e = os.Stat(filepath.Join(root, "network-leases.json")); !os.IsNotExist(e) {
			t.Fatal("failed old-container removal reserved new network")
		}
		if failStop && d.removeCount != 0 {
			t.Fatal("remove attempted after failed stop")
		}
		d.stopError, d.removeError = nil, nil
		req.Generation++
		if s, e = h.Do(context.Background(), req); e != nil || !s.Ready || s.Network == nil || s.InstanceID != old.InstanceID {
			t.Fatalf("retry did not complete preserved migration: %v", e)
		}
	}
}

func TestNetworkMigrationRequiresPrivatePrepareAndNewGeneration(t *testing.T) {
	h, _, _, d := legacyMigrationFixture(t)
	req := prepareRequest()
	req.MigrateNetwork = true
	if _, e := h.Do(context.Background(), req); errorCode(e) != "network_migration_requires_new_generation" {
		t.Fatalf("same generation migrated legacy instance: %v", e)
	}
	req.Operation, req.Generation = "apply-config", 2
	if _, e := h.Do(context.Background(), req); errorCode(e) != "network_migration_operation_invalid" {
		t.Fatalf("non-prepare migration accepted: %v", e)
	}
	if d.removeCount != 0 {
		t.Fatal("invalid migration removed container")
	}
}

func TestNetworkMigrationLegacyUnknownCreateDoesNotAllocateBeforeExplicitMigration(t *testing.T) {
	h, root, old, d := legacyMigrationFixture(t)
	// Simulate the pre-allocator helper losing the Docker create response:
	// credential projection is durable, but ContainerID is not yet saved.
	old.ContainerID = ""
	old.Ready = false
	if e := h.save(filepath.Join(root, old.InstanceID), old); e != nil {
		t.Fatal(e)
	}
	req := prepareRequest()
	req.Generation = 2
	s, e := h.Do(context.Background(), req)
	if e != nil || s.Network != nil || s.InstanceID != old.InstanceID || s.ClientIdentity.DeviceID != old.ClientIdentity.DeviceID {
		t.Fatalf("legacy unknown-create retry was silently reassigned: %v", e)
	}
	if _, e = os.Stat(filepath.Join(root, "network-leases.json")); !os.IsNotExist(e) {
		t.Fatal("legacy unknown-create retry reserved a conflicting lease")
	}
	if d.removeCount != 0 {
		t.Fatal("ordinary retry deleted legacy container")
	}
	req.Generation, req.MigrateNetwork = 3, true
	s, e = h.Do(context.Background(), req)
	if e != nil || !s.Ready || s.Network == nil || d.removeCount != 1 || s.InstanceID != old.InstanceID || s.Identity != old.Identity || s.ClientIdentity.DeviceID != old.ClientIdentity.DeviceID {
		t.Fatalf("explicit migration did not allocate after confirmed removal: %v", e)
	}
}
