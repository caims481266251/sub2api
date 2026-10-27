package vmruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// Observe the manifest as the external stop begins, when a crash must already
// leave the old generation and new identity fenced together.
type desktopMigrationFenceDriver struct {
	*fakeDriver
	helper  *Helper
	root    string
	stopped *State
	readErr error
}

type desktopRemovalInspectDriver struct {
	*fakeDriver
	inspections int
}

func (d *desktopRemovalInspectDriver) Inspect(ctx context.Context, state State, dir string) (Observation, error) {
	d.inspections++
	return d.fakeDriver.Inspect(ctx, state, dir)
}

func (d *desktopMigrationFenceDriver) Stop(ctx context.Context, state State) error {
	d.stopped, d.readErr = d.helper.read(filepath.Join(d.root, state.InstanceID))
	return d.fakeDriver.Stop(ctx, state)
}

func readyWithOldDesktopUA(t *testing.T, driver Driver) (*Helper, string, *State, Request) {
	t.Helper()
	h, root := openTest(t, driver)
	req := desktopIdentityRequest()
	state, err := h.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	state.ClientIdentity.UserAgentTemplate = "desktop-v1/0.150.0/Mac OS/14.0/arm64"
	state.ClientIdentity.UserAgent = desktopUserAgentCatalog()[state.ClientIdentity.UserAgentTemplate]
	if err := h.save(filepath.Join(root, state.InstanceID), state); err != nil {
		t.Fatal(err)
	}
	req.Operation = "apply-config"
	req.InstanceID = state.InstanceID
	req.Generation = state.Generation + 1
	return h, root, state, req
}

func TestDesktopUAMigrationFencesBeforeStopping(t *testing.T) {
	driver := &desktopMigrationFenceDriver{fakeDriver: &fakeDriver{}}
	h, root, original, req := readyWithOldDesktopUA(t, driver)
	driver.helper, driver.root = h, root
	old := *original.ClientIdentity
	state, err := h.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if driver.readErr != nil || driver.stopped == nil || driver.stopCount != 1 {
		t.Fatalf("stop did not observe the migration fence: %v", driver.readErr)
	}
	view := driver.stopped
	if view.Generation != req.Generation || view.Ready || view.Phase != "apply-config" ||
		view.ClientIdentity.DeviceID != old.DeviceID || view.ClientIdentity.UserAgent == old.UserAgent ||
		DesktopUserAgentNeedsMigration(view.ClientIdentity) {
		t.Fatalf("identity and fence not persisted together: %+v", view)
	}
	if !state.Ready || *state.ClientIdentity != *view.ClientIdentity || state.Identity != original.Identity ||
		state.InstanceID != original.InstanceID || state.Spec != original.Spec {
		t.Fatal("migration changed the stable identity or did not become ready")
	}
	// A same-generation prepare is an inspect, not a second rotation or stop.
	req.Operation = "prepare"
	state, err = h.Do(context.Background(), req)
	if err != nil || driver.stopCount != 1 || *state.ClientIdentity != *view.ClientIdentity {
		t.Fatalf("ready replay rotated identity or stopped the container: %v", err)
	}
}

func TestDesktopUAMigrationPoolExhaustionPreservesRunningState(t *testing.T) {
	driver := &fakeDriver{}
	h, root, original, req := readyWithOldDesktopUA(t, driver)
	// Create durable tombstones for the complete allocation pool. The test
	// fixture writes directly so thousands of individual fsyncs are unnecessary.
	for id, ua := range desktopUserAgents() {
		entry := *original
		entry.InstanceID = uuid.NewString()
		entry.AccountID++
		entry.ClientIdentity = &ClientIdentity{DeviceID: uuid.NewString(), UserAgentTemplate: id, UserAgent: ua}
		entry.DeleteRequested = true
		dir := filepath.Join(root, entry.InstanceID)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := filepath.Join(root, original.InstanceID, "manifest.json")
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	// A proxy change must also remain uncommitted on allocation failure.
	req.Spec.ProxyID++
	req.Spec.ProxyRevision = "px2"
	req.Projection.ProxyURL = "http://192.0.2.2:3128"
	returned, err := h.Do(context.Background(), req)
	if errorCode(err) != "client_user_agent_pool_exhausted" {
		t.Fatalf("expected exhausted pool, got %v", err)
	}
	after, err := os.ReadFile(manifest)
	if err != nil || string(after) != string(before) || returned.Generation != original.Generation ||
		!returned.Ready || returned.Spec != original.Spec || *returned.ClientIdentity != *original.ClientIdentity {
		t.Fatalf("failed allocation changed the original manifest or state: %v", err)
	}
	if driver.stopCount != 0 || driver.ensureCount != 1 {
		t.Fatal("failed allocation changed the existing container")
	}
}

func TestDesktopUAInvalidIdentityPreservesRunningState(t *testing.T) {
	for _, corrupt := range []string{"profile", "device", "partial"} {
		t.Run(corrupt, func(t *testing.T) {
			driver := &fakeDriver{}
			h, root, original, req := readyWithOldDesktopUA(t, driver)
			switch corrupt {
			case "profile":
				original.ClientIdentity.UserAgent = "forged"
			case "device":
				original.ClientIdentity.DeviceID = "invalid"
			case "partial":
				original.ClientIdentity.UserAgent = ""
			}
			manifest := filepath.Join(root, original.InstanceID, "manifest.json")
			if err := h.save(filepath.Dir(manifest), original); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			returned, err := h.Do(context.Background(), req)
			if errorCode(err) != "client_user_agent_invalid" && errorCode(err) != "client_identity_invalid" {
				t.Fatalf("expected identity validation failure, got %v", err)
			}
			after, readErr := os.ReadFile(manifest)
			if readErr != nil || string(after) != string(before) || returned != nil {
				t.Fatalf("invalid identity changed the original manifest or state: %v", readErr)
			}
			if driver.stopCount != 0 || driver.ensureCount != 1 {
				t.Fatal("invalid identity changed the existing container")
			}
		})
	}
}

func TestDesktopUAMigrationRequiresProjectionBeforeFencing(t *testing.T) {
	driver := &fakeDriver{}
	h, root, original, req := readyWithOldDesktopUA(t, driver)
	manifest := filepath.Join(root, original.InstanceID, "manifest.json")
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	req.Projection = nil
	returned, err := h.Do(context.Background(), req)
	if errorCode(err) != "credential_projection_required" {
		t.Fatalf("expected missing projection rejection, got %v", err)
	}
	after, readErr := os.ReadFile(manifest)
	if readErr != nil || string(after) != string(before) || returned != nil || driver.stopCount != 0 {
		t.Fatalf("missing projection changed the running instance: %v", readErr)
	}
}

func TestDesktopUALifecycleRecyclesOnlyAfterConfirmedRemoval(t *testing.T) {
	driver := &desktopRemovalInspectDriver{fakeDriver: &fakeDriver{}}
	h, _ := openTest(t, driver)
	firstReq := desktopIdentityRequest()
	first, err := h.Do(context.Background(), firstReq)
	if err != nil {
		t.Fatal(err)
	}
	ua := first.ClientIdentity.UserAgent
	driver.removeError = fault("docker_operation_unknown")
	remove := Request{Version: ProtocolVersion, Operation: "remove", AccountID: first.AccountID, InstanceID: first.InstanceID, Generation: first.Generation + 1}
	failed, err := h.Do(context.Background(), remove)
	if err == nil || failed == nil || failed.Phase != "error" {
		t.Fatal("expected unconfirmed container removal")
	}
	secondReq := desktopIdentityRequest()
	secondReq.AccountID++
	second, err := h.Do(context.Background(), secondReq)
	if err != nil || second.ClientIdentity.UserAgent == ua {
		t.Fatalf("unconfirmed removal released its UA: %v", err)
	}
	driver.removeError = nil
	remove.Generation = failed.Generation + 1
	removed, err := h.Do(context.Background(), remove)
	if err != nil || removed.Phase != "removed" || removed.ContainerID != "" {
		t.Fatalf("container removal did not complete: %v", err)
	}
	thirdReq := desktopIdentityRequest()
	thirdReq.AccountID += 2
	third, err := h.Do(context.Background(), thirdReq)
	if err != nil || third.ClientIdentity.UserAgent != ua || third.ClientIdentity.DeviceID == first.ClientIdentity.DeviceID {
		t.Fatalf("confirmed removal did not release its UA: %v", err)
	}
	stops, removals := driver.stopCount, driver.removeCount
	remove.Generation = removed.Generation + 1
	again, err := h.Do(context.Background(), remove)
	if err != nil || again.Phase != "removed" || driver.stopCount != stops || driver.removeCount != removals {
		t.Fatalf("repeated removal reopened a released reservation: %v", err)
	}
	inspections := driver.inspections
	view, err := h.Do(context.Background(), Request{Version: ProtocolVersion, Operation: "inspect", AccountID: first.AccountID, InstanceID: first.InstanceID})
	if err != nil || view.Phase != "removed" || driver.inspections != inspections {
		t.Fatalf("inspection reopened a released reservation: %v", err)
	}
	fourthReq := desktopIdentityRequest()
	fourthReq.AccountID += 3
	fourth, err := h.Do(context.Background(), fourthReq)
	if err != nil || fourth.ClientIdentity.UserAgent == ua || fourth.ClientIdentity.UserAgent == second.ClientIdentity.UserAgent {
		t.Fatalf("repeated removal disturbed existing UA assignments: %v", err)
	}
}

func TestDesktopUAInspectDoesNotReleaseUnconfirmedRemoval(t *testing.T) {
	driver := &fakeDriver{}
	h, root := openTest(t, driver)
	firstReq := desktopIdentityRequest()
	first, err := h.Do(context.Background(), firstReq)
	if err != nil {
		t.Fatal(err)
	}
	ua := first.ClientIdentity.UserAgent
	// A create or remove may have an unknown outcome even without a stored ID.
	first.DeleteRequested, first.Phase, first.ContainerID = true, "error", ""
	if err := h.save(filepath.Join(root, first.InstanceID), first); err != nil {
		t.Fatal(err)
	}
	view, err := h.Do(context.Background(), Request{Version: ProtocolVersion, Operation: "inspect", AccountID: first.AccountID, InstanceID: first.InstanceID})
	if err != nil || view.Phase != "deleting" || !view.DeleteRequested {
		t.Fatalf("inspection treated unknown removal as confirmed: %v", err)
	}
	secondReq := desktopIdentityRequest()
	secondReq.AccountID++
	second, err := h.Do(context.Background(), secondReq)
	if err != nil || second.ClientIdentity.UserAgent == ua {
		t.Fatalf("unconfirmed removal released its UA after inspection: %v", err)
	}
}
