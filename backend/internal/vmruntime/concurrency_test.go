package vmruntime

import (
	"context"
	"testing"
	"time"
)

type blockingDriver struct{ started, release chan struct{} }

func (d *blockingDriver) Preflight(_ context.Context, s State) error {
	if s.AccountID == 42 {
		close(d.started)
		<-d.release
	}
	return nil
}
func (d *blockingDriver) Ensure(_ context.Context, s State, _ string) (string, error) {
	return s.InstanceID, nil
}
func (d *blockingDriver) Inspect(_ context.Context, s State, _ string) (Observation, error) {
	return Observation{Running: true, KVMEnabled: true, KernelReady: true, GuestBootID: "boot", CredentialRevision: s.DesiredCredentialRevision, ProxyRevision: s.Spec.ProxyRevision}, nil
}
func (d *blockingDriver) Apply(ctx context.Context, s State, dir string) (Observation, error) {
	return d.Inspect(ctx, s, dir)
}
func (d *blockingDriver) Stop(context.Context, State) error   { return nil }
func (d *blockingDriver) Remove(context.Context, State) error { return nil }

func TestSlowBootDoesNotBlockOtherAccountsOrInventory(t *testing.T) {
	d := &blockingDriver{started: make(chan struct{}), release: make(chan struct{})}
	h, _ := openTest(t, d)
	req := prepareRequest()
	req.AccountID = 43
	if _, err := h.Do(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	bootDone := make(chan error, 1)
	go func() { _, err := h.Do(context.Background(), prepareRequest()); bootDone <- err }()
	<-d.started
	defer func() {
		close(d.release)
		if err := <-bootDone; err != nil {
			t.Error(err)
		}
	}()
	inspected := make(chan error, 1)
	go func() {
		_, err := h.Do(context.Background(), Request{Version: 1, Operation: "inspect", AccountID: 43})
		if err == nil {
			_, err = h.Inventory()
		}
		inspected <- err
	}()
	select {
	case err := <-inspected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated account blocked behind guest boot")
	}
}
