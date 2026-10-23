package vmruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestClientIdentityPersistsAcrossRestartAndCredentialRotation(t *testing.T) {
	driver := &fakeDriver{}
	h, root := openTest(t, driver)
	s, e := h.Do(context.Background(), prepareRequest())
	if e != nil || s.ClientIdentity == nil {
		t.Fatalf("prepare did not bind client identity: %v", e)
	}
	device := s.ClientIdentity.DeviceID
	if parsed, e := uuid.Parse(device); e != nil || parsed.String() != device {
		t.Fatal("client identity not canonical UUID")
	}
	if e = h.Close(); e != nil {
		t.Fatal(e)
	}
	h, e = OpenHelper(root, driver)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	req := prepareRequest()
	req.Operation, req.Generation = "apply-config", 2
	req.Projection.Revision = "c2"
	s, e = h.Do(context.Background(), req)
	if e != nil || s.ClientIdentity == nil || s.ClientIdentity.DeviceID != device {
		t.Fatalf("credential update changed client identity: %v", e)
	}
	other := prepareRequest()
	other.AccountID++
	second, e := h.Do(context.Background(), other)
	if e != nil || second.ClientIdentity == nil || second.ClientIdentity.DeviceID == device {
		t.Fatalf("accounts did not receive independent identities: %v", e)
	}
}

func TestClientIdentityMigratesExistingInstallationAndRejectsMismatch(t *testing.T) {
	driver := &fakeDriver{}
	h, root := openTest(t, driver)
	req, state, e := h.resolve(prepareRequest())
	if e != nil {
		t.Fatal(e)
	}
	kvm := filepath.Join(root, state.InstanceID, "kvm")
	if e = os.MkdirAll(kvm, 0700); e != nil {
		t.Fatal(e)
	}
	device := uuid.NewString()
	path := filepath.Join(kvm, "codex-installation-id")
	if e = os.WriteFile(path, []byte(strings.ToUpper(device)+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	state, e = h.Do(context.Background(), req)
	if e != nil || state.ClientIdentity == nil || state.ClientIdentity.DeviceID != device {
		t.Fatalf("existing installation identity was not migrated: %v", e)
	}
	if e = os.WriteFile(path, []byte(uuid.NewString()), 0600); e != nil {
		t.Fatal(e)
	}
	ensures := driver.ensureCount
	req.Generation = 2
	if _, e = h.Do(context.Background(), req); errorCode(e) != "client_identity_conflict" || driver.ensureCount != ensures {
		t.Fatalf("conflicting identity reached driver: %v", e)
	}
	stored, e := h.read(filepath.Join(root, state.InstanceID))
	if e != nil || stored.ClientIdentity.DeviceID != device {
		t.Fatal("mismatch silently replaced persisted identity")
	}
}

func TestClientIdentityIsDurableBeforeFailedExternalCreate(t *testing.T) {
	driver := &fakeDriver{ensureError: fault("docker_operation_unknown")}
	h, root := openTest(t, driver)
	s, e := h.Do(context.Background(), prepareRequest())
	if e == nil || s.ClientIdentity == nil {
		t.Fatal("expected failed create with assigned identity")
	}
	stored, e := h.read(filepath.Join(root, s.InstanceID))
	if e != nil || stored.ClientIdentity == nil || *stored.ClientIdentity != *s.ClientIdentity {
		t.Fatal("failed external create lost durable client identity")
	}
}

func TestClientIdentityRejectsInvalidOrSymlinkedInstallation(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		driver := &fakeDriver{}
		h, root := openTest(t, driver)
		req, state, e := h.resolve(prepareRequest())
		if e != nil {
			t.Fatal(e)
		}
		kvm := filepath.Join(root, state.InstanceID, "kvm")
		if e = os.MkdirAll(kvm, 0700); e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(kvm, "codex-installation-id")
		want := "client_identity_invalid"
		if symlink {
			want = "symlink_forbidden"
			e = os.Symlink(filepath.Join(root, "not-read"), path)
		} else {
			e = os.WriteFile(path, []byte("invalid-id"), 0600)
		}
		if e != nil {
			t.Fatal(e)
		}
		if _, e = h.Do(context.Background(), req); errorCode(e) != want || driver.ensureCount != 0 {
			t.Fatalf("invalid identity reached driver: %v", e)
		}
	}
}

func TestKernelSOCKSProjectionUsesRemoteDNSAndAcceptsPrepareReplay(t *testing.T) {
	driver := &fakeDriver{}
	h, root := openTest(t, driver)
	req := prepareRequest()
	req.Projection.ProxyURL = "socks5://fixture:local-only@192.0.2.1:1080"
	s, e := h.Do(context.Background(), req)
	if e != nil {
		t.Fatal(e)
	}
	var projected struct {
		URL string `json:"url"`
	}
	raw, e := os.ReadFile(filepath.Join(root, s.InstanceID, "credentials", "proxy.json"))
	if e != nil || json.Unmarshal(raw, &projected) != nil || projected.URL != "socks5h://fixture:local-only@192.0.2.1:1080" {
		t.Fatal("SOCKS projection did not request remote DNS")
	}
	if req.Projection.ProxyURL != "socks5://fixture:local-only@192.0.2.1:1080" {
		t.Fatal("projection mutated account input")
	}
	ensures := driver.ensureCount
	if _, e = h.Do(context.Background(), req); e != nil || driver.ensureCount != ensures {
		t.Fatalf("normalized same-generation replay restarted instance: %v", e)
	}
}
