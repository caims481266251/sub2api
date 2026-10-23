//go:build !windows

package vmruntime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseOperationContract(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "s2-release-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "helper")
	l, err := net.Listen("unix", socket+".update")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/v1/prepare" || req["version"] != "2.15.4" {
			t.Error("invalid request")
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": req["version"]})
	})}
	go server.Serve(l)
	defer server.Close()
	if err := ReleaseOperation(context.Background(), socket, "prepare", "2.15.4"); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOperation(context.Background(), socket, "prepare", "../../other"); err == nil {
		t.Fatal("invalid version accepted")
	}
	if err := ReleaseOperation(context.Background(), socket, "shell", "2.15.4"); err == nil {
		t.Fatal("invalid action accepted")
	}
}
func TestReleaseOperationUnavailableFailsClosed(t *testing.T) {
	if err := ReleaseOperation(context.Background(), filepath.Join(t.TempDir(), "missing"), "prepare", "2.15.4"); err == nil {
		t.Fatal("missing runtime agent accepted")
	}
}
