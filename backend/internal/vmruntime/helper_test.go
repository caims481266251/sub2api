package vmruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeDriver struct {
	ensureError                         error
	ensureCount, stopCount, removeCount int
	removeError                         error
	observed                            Observation
}

func (f *fakeDriver) Preflight(context.Context, State) error { return nil }
func (f *fakeDriver) Ensure(_ context.Context, s State, _ string) (string, error) {
	f.ensureCount++
	if f.ensureError != nil {
		return "", f.ensureError
	}
	return "container-" + s.InstanceID, nil
}
func (f *fakeDriver) Inspect(context.Context, State, string) (Observation, error) {
	return f.observed, nil
}
func (f *fakeDriver) Apply(_ context.Context, s State, _ string) (Observation, error) {
	f.observed = Observation{Running: true, KVMEnabled: true, KernelReady: true, GuestBootID: "boot-1", CredentialRevision: s.DesiredCredentialRevision, ProxyRevision: s.Spec.ProxyRevision}
	return f.observed, nil
}
func (f *fakeDriver) Stop(context.Context, State) error   { f.stopCount++; return nil }
func (f *fakeDriver) Remove(context.Context, State) error { f.removeCount++; return f.removeError }
func prepareRequest() Request {
	return Request{Version: 1, Operation: "prepare", AccountID: 42, Generation: 1, Spec: &Spec{Platform: "claude", Template: "fixed", CPU: 1, MemoryMiB: 1024, DiskGiB: 8, ProxyID: 7, ProxyRevision: "px1"}, Projection: &Projection{Revision: "c1", ProxyURL: "http://192.0.2.1:3128", Credential: json.RawMessage(`{"claudeAiOauth":{"accessToken":"test-only"}}`)}}
}
func openTest(t *testing.T, f Driver) (*Helper, string) {
	t.Helper()
	root := t.TempDir()
	h, err := OpenHelper(root, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h, root
}

func TestAccountLookupAndPersistentIdentity(t *testing.T) {
	f := &fakeDriver{}
	h, root := openTest(t, f)
	req := prepareRequest()
	s, err := h.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if s.Capacity != 0 || !s.Ready || s.InternalToken == "" || s.InstanceID == "" {
		t.Fatalf("not ready: %+v", s)
	}
	identity := s.Identity
	id := s.InstanceID
	token := s.InternalToken
	manifest, err := os.ReadFile(filepath.Join(root, id, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), token) {
		t.Fatal("token leaked into manifest")
	}
	h.Close()
	h2, err := OpenHelper(root, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	s, err = h2.Do(context.Background(), Request{Version: 1, Operation: "inspect", AccountID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if s.InstanceID != id || s.Identity != identity || s.InternalToken != token || s.Generation != 1 {
		t.Fatal("identity or token changed after restart")
	}
}

func TestDeleteFailureRetainsIntentAndData(t *testing.T) {
	f := &fakeDriver{removeError: fault("daemon_unavailable")}
	h, root := openTest(t, f)
	s, err := h.Do(context.Background(), prepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	s, err = h.Do(context.Background(), Request{Version: 1, Operation: "remove", AccountID: 42, InstanceID: s.InstanceID, Generation: 2})
	if err == nil || !s.DeleteRequested || s.Ready || !s.DataRetained {
		t.Fatal("delete failure lost intent")
	}
	if _, err = os.Stat(filepath.Join(root, s.InstanceID, "credentials", "credential.json")); err != nil {
		t.Fatal("credentials were purged")
	}
	req := prepareRequest()
	req.Generation = 3
	_, err = h.Do(context.Background(), req)
	if errorCode(err) != "instance_deleting" {
		t.Fatalf("deletion resurrected: %v", err)
	}
}

func TestFencingAndImmutableDisk(t *testing.T) {
	h, _ := openTest(t, &fakeDriver{})
	req := prepareRequest()
	req.Generation = 2
	s, err := h.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Generation = 1
	req.InstanceID = s.InstanceID
	if _, err = h.Do(context.Background(), req); errorCode(err) != "stale_generation" {
		t.Fatal(err)
	}
	req.Generation = 3
	req.Spec.DiskGiB = 16
	if _, err = h.Do(context.Background(), req); errorCode(err) != "immutable_spec_mismatch" {
		t.Fatal(err)
	}
}

func TestProjectionRejectsRefreshAndRevisionReuse(t *testing.T) {
	f := &fakeDriver{}
	h, _ := openTest(t, f)
	req := prepareRequest()
	req.Projection.Credential = json.RawMessage(`{"nested":{"refreshToken":"never-accept"}}`)
	if _, err := h.Do(context.Background(), req); errorCode(err) != "refresh_credential_forbidden" {
		t.Fatal(err)
	}
	req = prepareRequest()
	if _, err := h.Do(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Projection.Credential = json.RawMessage(`{"claudeAiOauth":{"accessToken":"changed"}}`)
	if _, err := h.Do(context.Background(), req); errorCode(err) != "projection_revision_conflict" {
		t.Fatal(err)
	}
	if f.stopCount != 0 {
		t.Fatal("rejected replay stopped the live guest")
	}
}

func TestLockAndArtifactVerification(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	defer h.Close()
	if _, err := OpenHelper(root, &fakeDriver{}); errorCode(err) != "helper_already_running" {
		t.Fatal(err)
	}
	p := filepath.Join(root, "payload")
	b := []byte("fixture only")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	if err := VerifyArtifact(p, digest); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(p, strings.Repeat("0", 64)); errorCode(err) != "artifact_digest_mismatch" {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(link, digest); errorCode(err) != "symlink_forbidden" {
		t.Fatal(err)
	}
}

func TestUnsupportedHostNeverInvokesDocker(t *testing.T) {
	d := &DockerDriver{HostOS: "darwin", HostArch: "arm64"}
	if err := d.Preflight(context.Background(), State{}); errorCode(err) != "linux_amd64_required" {
		t.Fatal(err)
	}
}

func TestMissingPoCGateNeverStartsRuntime(t *testing.T) {
	d := &DockerDriver{HostOS: "linux", HostArch: "amd64", Templates: map[string]Template{"fixed": {Image: "example.invalid/kvm@sha256:" + strings.Repeat("a", 64)}}}
	if err := d.Preflight(context.Background(), State{Spec: Spec{Template: "fixed"}}); errorCode(err) != "template_poc_not_verified" {
		t.Fatal(err)
	}
}

func TestPrepareReplayAndProxyRotation(t *testing.T) {
	f := &fakeDriver{}
	h, _ := openTest(t, f)
	req := prepareRequest()
	first, err := h.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := h.Do(context.Background(), req)
	if err != nil || !replay.Ready || replay.Identity != first.Identity || f.ensureCount != 1 || f.stopCount != 0 {
		t.Fatalf("replay changed runtime: %v", err)
	}
	req.Operation = "apply-config"
	req.Spec.ProxyID = 8
	req.Spec.ProxyRevision = "px2"
	req.Projection.ProxyURL = "http://192.0.2.2:3128"
	if _, err = h.Do(context.Background(), req); errorCode(err) != "immutable_spec_mismatch" {
		t.Fatal(err)
	}
	req.Generation = 2
	req.Projection.Revision = "c2"
	req.Projection.Credential = json.RawMessage(`{"claudeAiOauth":{"accessToken":"rotated"}}`)
	updated, err := h.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Ready || updated.AppliedProxyRevision != "px2" || updated.Identity != first.Identity || f.stopCount != 1 {
		t.Fatalf("rotation not applied safely: %+v", updated)
	}
}

func TestMissingLiveTokenFailsClosed(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	s, err := h.Do(context.Background(), prepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(root, s.InstanceID, "run", "internal.token")); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Do(context.Background(), Request{Version: 1, Operation: "inspect", AccountID: 42}); errorCode(err) != "internal_token_missing" {
		t.Fatal(err)
	}
}

func TestStopRequiresNewGeneration(t *testing.T) {
	f := &fakeDriver{}
	h, _ := openTest(t, f)
	s, err := h.Do(context.Background(), prepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Version: 1, Operation: "stop", AccountID: 42, InstanceID: s.InstanceID, Generation: 1}
	if _, err = h.Do(context.Background(), req); errorCode(err) != "mutation_requires_new_generation" || f.stopCount != 0 {
		t.Fatal(err)
	}
	req.Generation = 2
	stopped, err := h.Do(context.Background(), req)
	if err != nil || stopped.Ready || stopped.Generation != 2 || f.stopCount != 1 {
		t.Fatalf("stop failed: %v", err)
	}
}

func TestUnknownCreationRequiresStopBeforeRecovery(t *testing.T) {
	f := &fakeDriver{ensureError: fault("docker_operation_unknown")}
	h, _ := openTest(t, f)
	req := prepareRequest()
	failed, err := h.Do(context.Background(), req)
	if err == nil || failed == nil || failed.ContainerID != "" || failed.DesiredCredentialRevision == "" {
		t.Fatalf("unknown creation lost state: %v", err)
	}
	if _, err = h.Do(context.Background(), req); errorCode(err) != "mutation_requires_new_generation" {
		t.Fatal(err)
	}
	req.Generation = 2
	f.ensureError = nil
	recovered, err := h.Do(context.Background(), req)
	if err != nil || !recovered.Ready || f.stopCount != 1 {
		t.Fatalf("unknown container not stopped before recovery: %v", err)
	}
}
