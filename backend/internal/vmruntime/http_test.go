package vmruntime

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestUDSLifecycleAndPrivateInventory(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	socketDir, err := os.MkdirTemp("/tmp", "vmr-uds-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	listener, err := net.Listen("unix", filepath.Join(socketDir, "helper.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: h.Handler()}
	done := make(chan struct{})
	go func() { defer close(done); server.Serve(listener) }()
	defer func() { server.Close(); <-done }()
	client := NewClient(filepath.Join(socketDir, "helper.sock"))
	defer client.CloseIdleConnections()
	ctx := context.Background()
	if _, err = client.Do(ctx, Request{Operation: "inspect", AccountID: 42}); errorCode(err) != "instance_not_found" {
		t.Fatalf("initial lookup: %v", err)
	}
	states, err := client.Inventory(ctx)
	if err != nil || len(states) != 0 {
		t.Fatalf("empty inventory: %v", err)
	}
	s, err := client.Do(ctx, prepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	states, err = client.Inventory(ctx)
	if err != nil || len(states) != 1 || states[0].InternalToken != "" || states[0].Ready || states[0].InstanceID != s.InstanceID {
		t.Fatalf("unsafe inventory: %v", err)
	}
	if _, err = client.Do(ctx, Request{Operation: "stop", AccountID: 42, Generation: 2}); err != nil {
		t.Fatal(err)
	}
	removed, err := client.Do(ctx, Request{Operation: "remove", AccountID: 42, Generation: 3})
	if err != nil || !removed.DeleteRequested || removed.Phase != "removed" {
		t.Fatalf("remove: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, s.InstanceID, "credentials", "credential.json")); err != nil {
		t.Fatal("remove purged data")
	}
	states, err = client.Inventory(ctx)
	if err != nil || len(states) != 1 || !states[0].DeleteRequested {
		t.Fatal("tombstone missing from inventory")
	}
	if _, err = client.Do(ctx, prepareRequest()); err == nil {
		t.Fatal("removed account resurrected")
	}
}
