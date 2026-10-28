package vmruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type profileCommands struct {
	calls        [][]string
	fail         bool
	host         string
	imageMissing bool
}

func (f *profileCommands) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "image":
		if f.imageMissing {
			return nil, fault("private_image_diagnostic")
		}
		return []byte(`{"Os":"linux","Architecture":"amd64"}`), nil
	case "info":
		if f.host != "" {
			return []byte(f.host), nil
		}
		return []byte("amd64"), nil
	case "run":
		if args[len(args)-1] == "--payload" {
			if f.fail {
				return nil, fault("private_probe_diagnostic")
			}
			return json.Marshal(map[string]any{"profile_id": args[len(args)-2], "ok": true})
		}
	}
	return nil, nil
}

func TestSystemProfileCatalogVerifiedOnceAndSanitized(t *testing.T) {
	root := t.TempDir()
	kernel := filepath.Join(root, "usr/local/bin/kin-codex-kernel")
	if err := os.MkdirAll(filepath.Dir(kernel), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kernel, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("fixture"))
	commands := &profileCommands{}
	driver := &DockerDriver{Commands: commands, Templates: map[string]Template{
		"ubuntu-24.04": {Runtime: "container-v1", Image: "sha256:" + strings.Repeat("a", 64), PayloadDir: root, Artifacts: map[string]string{kernel: hex.EncodeToString(sum[:])}},
	}}
	driver.VerifySystemProfiles(context.Background())
	count := len(commands.calls)
	first := driver.SystemProfiles()
	if len(first) != 5 || !first[1].Available || first[0].UnavailableReason != "profile_not_installed" {
		t.Fatalf("unexpected catalog: %+v", first)
	}
	first[1].Available = false
	if !driver.SystemProfiles()[1].Available || len(commands.calls) != count {
		t.Fatal("catalog read mutated cache or invoked Docker")
	}
	commands.fail = true
	driver.VerifySystemProfiles(context.Background())
	if got := driver.SystemProfiles()[1]; got.Available || got.UnavailableReason != "profile_validation_failed" {
		t.Fatalf("unsafe failure: %+v", got)
	}
	last := commands.calls[len(commands.calls)-1]
	if len(last) != 3 || last[0] != "rm" || last[1] != "-f" || !strings.HasPrefix(last[2], "sub2-profile-check-") {
		t.Fatalf("validation container not cleaned: %v", last)
	}
	commands.fail = false
	commands.imageMissing = true
	driver.VerifySystemProfiles(context.Background())
	if got := driver.SystemProfiles()[1].UnavailableReason; got != "profile_image_missing" {
		t.Fatalf("missing image: %s", got)
	}
	commands.imageMissing = false
	commands.host = "aarch64"
	driver.VerifySystemProfiles(context.Background())
	for _, index := range []int{0, 1, 3, 4} {
		if got := driver.SystemProfiles()[index].UnavailableReason; got != "architecture_not_supported" {
			t.Fatalf("wrong architecture %d: %s", index, got)
		}
	}
	commands.host = "amd64"
	template := driver.Templates["ubuntu-24.04"]
	template.Image = "mutable:latest"
	driver.Templates["ubuntu-24.04"] = template
	driver.VerifySystemProfiles(context.Background())
	if got := driver.SystemProfiles()[1]; got.Available || got.UnavailableReason != "profile_validation_failed" {
		t.Fatalf("mutable image allowed: %+v", got)
	}
}

func TestSystemProfilesEndpointIsReadOnlyAndContainsNoPrivateImageFields(t *testing.T) {
	driver := &DockerDriver{}
	helper := &Helper{driver: driver}
	request := httptest.NewRequest(http.MethodGet, "/v1/system-profiles", nil)
	response := httptest.NewRecorder()
	helper.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("catalog request: %d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "ubuntu-24.04") || strings.Contains(body, "payload_dir") || strings.Contains(body, "sha256:") {
		t.Fatalf("unsafe catalog: %s", body)
	}
	response = httptest.NewRecorder()
	helper.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/system-profiles", nil))
	if response.Code < 400 {
		t.Fatal("catalog mutation accepted")
	}
}

func TestSystemProfilesConcurrentReadersDuringVerification(t *testing.T) {
	driver := &DockerDriver{Commands: &profileCommands{}}
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				profiles := driver.SystemProfiles()
				if len(profiles) != 5 {
					t.Errorf("incomplete profile snapshot: %d", len(profiles))
					return
				}
				profiles[0].DisplayName = "caller-owned copy"
			}
		}()
	}
	for range 10 {
		driver.VerifySystemProfiles(t.Context())
	}
	readers.Wait()
	if driver.SystemProfiles()[0].DisplayName != "CentOS Stream 9" {
		t.Fatal("caller mutated shared profile catalog")
	}
}
