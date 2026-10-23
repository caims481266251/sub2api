package vmruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
