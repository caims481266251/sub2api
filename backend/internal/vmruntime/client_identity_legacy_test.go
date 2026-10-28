package vmruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Model the Docker driver's generation-bound receipt. Reading the manifest in
// Stop also checks the durable fence at the first external mutation boundary.
type legacyIdentityDriver struct {
	*fakeDriver
	helper            *Helper
	root              string
	stopped           *State
	readErr           error
	stopError         error
	appliedGeneration uint64
}

func (d *legacyIdentityDriver) Apply(ctx context.Context, s State, dir string) (Observation, error) {
	d.appliedGeneration = s.Generation
	return d.fakeDriver.Apply(ctx, s, dir)
}

func (d *legacyIdentityDriver) Inspect(ctx context.Context, s State, dir string) (Observation, error) {
	observation, err := d.fakeDriver.Inspect(ctx, s, dir)
	observation.KernelReady = observation.KernelReady && d.appliedGeneration == s.Generation
	return observation, err
}

func (d *legacyIdentityDriver) Stop(ctx context.Context, s State) error {
	d.stopped, d.readErr = d.helper.read(filepath.Join(d.root, s.InstanceID))
	if err := d.fakeDriver.Stop(ctx, s); err != nil {
		return err
	}
	return d.stopError
}

func readyWithLegacyClientIdentity(t *testing.T) (*Helper, string, *legacyIdentityDriver, *State, Request) {
	t.Helper()
	driver := &legacyIdentityDriver{fakeDriver: &fakeDriver{}}
	helper, root := openTest(t, driver)
	driver.helper, driver.root = helper, root
	req := desktopIdentityRequest()
	state, err := helper.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Decode the old on-disk field names, including a previously unknown
	// template. Restoring defaults must not depend on the removed UA catalog.
	raw, err := json.Marshal(map[string]string{
		"device_id":           state.ClientIdentity.DeviceID,
		"user_agent":          "legacy-client/0.1 (Legacy OS; architecture)",
		"user_agent_template": "retired-template",
	})
	if err != nil || json.Unmarshal(raw, state.ClientIdentity) != nil {
		t.Fatal("legacy client identity was not decoded")
	}
	if err := helper.save(filepath.Join(root, state.InstanceID), state); err != nil {
		t.Fatal(err)
	}
	req.Operation = "apply-config"
	req.InstanceID = state.InstanceID
	req.Generation = state.Generation + 1
	return helper, root, driver, state, req
}

func TestLegacyUserAgentInspectionPreservesPendingOverride(t *testing.T) {
	helper, root, driver, original, req := readyWithLegacyClientIdentity(t)
	want := *original.ClientIdentity
	for _, operation := range []string{"inspect", "prepare"} {
		readRequest := req
		readRequest.Operation, readRequest.Generation = operation, original.Generation
		if operation == "inspect" {
			readRequest.Projection = nil
		}
		state, err := helper.Do(context.Background(), readRequest)
		if err != nil || state == nil || !state.Ready || *state.ClientIdentity != want {
			t.Fatalf("%s cleared or changed an unapplied override: %v", operation, err)
		}
		stored, err := helper.read(filepath.Join(root, original.InstanceID))
		if err != nil || *stored.ClientIdentity != want || driver.stopCount != 0 {
			t.Fatalf("%s changed the durable identity or stopped the kernel: %v", operation, err)
		}
	}
}

func TestLegacyUserAgentResetPersistsFenceBeforeStopping(t *testing.T) {
	for _, operation := range []string{"prepare", "apply-config"} {
		t.Run(operation, func(t *testing.T) {
			helper, root, driver, original, req := readyWithLegacyClientIdentity(t)
			req.Operation = operation
			device := original.ClientIdentity.DeviceID
			state, err := helper.Do(context.Background(), req)
			if err != nil || state == nil || !state.Ready {
				t.Fatalf("restoring kernel defaults failed: %v", err)
			}
			fence := driver.stopped
			if driver.readErr != nil || fence == nil || driver.stopCount != 1 {
				t.Fatalf("stop did not observe the saved fence: %v", driver.readErr)
			}
			if fence.Generation != req.Generation || fence.Ready || fence.Phase != operation ||
				fence.ClientIdentity.DeviceID != device || fence.ClientIdentity.LegacyUserAgent != "" || fence.ClientIdentity.LegacyUserAgentTemplate != "" {
				t.Fatal("legacy override and new generation were not fenced together")
			}
			if *state.ClientIdentity != *fence.ClientIdentity || state.Identity != original.Identity || state.InstanceID != original.InstanceID || state.Spec != original.Spec {
				t.Fatal("restoring defaults changed the instance or device identity")
			}
			raw, err := os.ReadFile(filepath.Join(root, state.InstanceID, "manifest.json"))
			var manifest struct {
				ClientIdentity map[string]json.RawMessage `json:"client_identity"`
			}
			if err != nil || json.Unmarshal(raw, &manifest) != nil || len(manifest.ClientIdentity) != 1 || manifest.ClientIdentity["device_id"] == nil {
				t.Fatal("new manifest still stores a UA override")
			}
			req.Operation = "prepare"
			if _, err := helper.Do(context.Background(), req); err != nil || driver.stopCount != 1 {
				t.Fatalf("same-generation replay restarted the kernel: %v", err)
			}
		})
	}
}

func TestLegacyUserAgentResetRequiresProjectionBeforeMutation(t *testing.T) {
	helper, root, driver, original, req := readyWithLegacyClientIdentity(t)
	req.Projection = nil
	if _, err := helper.Do(context.Background(), req); errorCode(err) != "credential_projection_required" {
		t.Fatalf("reset without a projection was accepted: %v", err)
	}
	stored, err := helper.read(filepath.Join(root, original.InstanceID))
	if err != nil || stored.Generation != original.Generation || !stored.Ready || *stored.ClientIdentity != *original.ClientIdentity || driver.stopCount != 0 {
		t.Fatalf("rejected reset changed the running instance: %v", err)
	}
}

func TestLegacyUserAgentResetFailureCannotReadyOldReceipt(t *testing.T) {
	helper, root, driver, original, req := readyWithLegacyClientIdentity(t)
	driver.stopError = fault("docker_operation_unknown")
	state, err := helper.Do(context.Background(), req)
	if errorCode(err) != "docker_operation_unknown" || state == nil || state.Ready || state.Generation != req.Generation ||
		state.ClientIdentity.DeviceID != original.ClientIdentity.DeviceID || state.ClientIdentity.LegacyUserAgent != "" || state.ClientIdentity.LegacyUserAgentTemplate != "" {
		t.Fatalf("failed reset lost its durable fence: %v", err)
	}
	stored, err := helper.read(filepath.Join(root, original.InstanceID))
	if err != nil || stored.Ready || stored.Generation != req.Generation || *stored.ClientIdentity != *state.ClientIdentity {
		t.Fatalf("failed reset was not saved: %v", err)
	}
	state, err = helper.Do(context.Background(), Request{Version: ProtocolVersion, Operation: "inspect", AccountID: original.AccountID})
	if err != nil || state.Ready {
		t.Fatalf("old kernel receipt became ready after failed reset: %v", err)
	}
	driver.stopError = nil
	req.Generation++
	state, err = helper.Do(context.Background(), req)
	if err != nil || !state.Ready || state.ClientIdentity.DeviceID != original.ClientIdentity.DeviceID || state.ClientIdentity.LegacyUserAgent != "" || state.ClientIdentity.LegacyUserAgentTemplate != "" {
		t.Fatalf("reset did not recover without changing device identity: %v", err)
	}
}
