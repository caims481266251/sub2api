package vmruntime

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func oldDesktopUAState(t *testing.T, h *Helper, root string, accountID int64, version, osName, osVersion, arch string) (*State, string) {
	t.Helper()
	s, dir := uaState(t, h, root, accountID)
	id := "desktop-v1/" + version + "/" + osName + "/" + osVersion + "/" + arch
	ua, ok := desktopUserAgentCatalog()[id]
	if !ok {
		t.Fatalf("test profile not in legacy catalog: %s", id)
	}
	s.ClientIdentity.UserAgentTemplate, s.ClientIdentity.UserAgent = id, ua
	if err := h.save(dir, s); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestDesktopUACompatibleAllocationCatalog(t *testing.T) {
	candidates := desktopUserAgents()
	if len(candidates) < 999 {
		t.Fatalf("compatible pool has only %d profiles", len(candidates))
	}
	seen := make(map[string]bool, len(candidates))
	for id, ua := range candidates {
		parts := strings.Split(id, "/")
		if len(parts) != 5 || legacyDesktopClientVersion(parts[1]) || seen[ua] {
			t.Fatalf("invalid or duplicate allocation %q", id)
		}
		seen[ua] = true
	}
	for _, version := range []string{"0.150.0", "0.151.0", "0.152.0", "0.153.0", "0.154.0", "0.155.0", "0.156.0", "0.157.0", "0.158.0"} {
		id := "desktop-v1/" + version + "/Mac OS/14.0/arm64"
		ua := "codex_cli_rs/" + version + " (Mac OS 14.0; arm64)"
		if desktopUserAgentCatalog()[id] != ua || candidates[id] != "" {
			t.Fatalf("legacy template was removed or reallocated: %s", id)
		}
		if !DesktopUserAgentNeedsMigration(&ClientIdentity{UserAgentTemplate: id, UserAgent: ua}) {
			t.Fatalf("legacy template was not marked for migration: %s", id)
		}
	}
	for _, identity := range []*ClientIdentity{
		nil,
		{},
		{UserAgentTemplate: "desktop-v1/0.150.0/Mac OS/99.0/arm64"},
		{UserAgentTemplate: "desktop-v1/0.150.0/Mac OS/14.0/arm64", UserAgent: "forged"},
		{UserAgentTemplate: "desktop-v1/0.159.0/Mac OS/14.0/arm64", UserAgent: "codex_cli_rs/0.159.0 (Mac OS 14.0; arm64)"},
		{UserAgentTemplate: "desktop-v1/0.164.0/Mac OS/14.0/arm64", UserAgent: "codex_cli_rs/0.164.0 (Mac OS 14.0; arm64)"},
	} {
		if DesktopUserAgentNeedsMigration(identity) {
			t.Fatalf("invalid or compatible identity marked for migration: %+v", identity)
		}
	}
	if desktopUserAgentCatalog()["desktop-v1/0.150.0/Mac OS/14.8/arm64"] != "" {
		t.Fatal("new OS combination incorrectly accepted as a historical profile")
	}
	// The previous release issued these newer OS profiles with 0.155-0.158.
	// They must remain valid historical identities and migrate normally.
	for _, version := range []string{"0.155.0", "0.156.0", "0.157.0", "0.158.0"} {
		id := "desktop-v1/" + version + "/Mac OS/14.8/arm64"
		ua := "codex_cli_rs/" + version + " (Mac OS 14.8; arm64)"
		if desktopUserAgentCatalog()[id] != ua || !DesktopUserAgentNeedsMigration(&ClientIdentity{UserAgentTemplate: id, UserAgent: ua}) {
			t.Fatalf("previous release profile lost or not migrated: %s", id)
		}
		if desktopUserAgentCatalog()["desktop-v1/"+version+"/Mac OS/13.7.8/arm64"] != "" {
			t.Fatal("new Ventura profile accepted as a previously issued identity")
		}
	}
}

func TestDesktopUAAllocationPrefersNewestCompatibleClient(t *testing.T) {
	ids := sortedDesktopProfileIDs(desktopUserAgents())
	if !strings.HasPrefix(ids[0], "desktop-v1/0.164.0/") || !strings.HasPrefix(ids[len(ids)-1], "desktop-v1/0.159.0/") {
		t.Fatalf("allocation does not prefer newest supported client: %s / %s", ids[0], ids[len(ids)-1])
	}
	h, root := openTest(t, &fakeDriver{})
	s, dir := uaState(t, h, root, 1)
	if err := h.ensureClientIdentity(dir, s); err != nil || !strings.HasPrefix(s.ClientIdentity.UserAgent, "codex_cli_rs/0.164.0 ") {
		t.Fatalf("fresh account did not receive newest client: %v", err)
	}
}

func TestDesktopUALegacyMigrationPreservesDeviceAndProfile(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	a, dir := oldDesktopUAState(t, h, root, 1, "0.150.0", "Mac OS", "14.0", "arm64")
	device := a.ClientIdentity.DeviceID
	if err := h.ensureClientIdentity(dir, a); err != nil {
		t.Fatal(err)
	}
	want := "desktop-v1/0.164.0/Mac OS/14.0/arm64"
	if a.ClientIdentity.DeviceID != device || a.ClientIdentity.UserAgentTemplate != want || DesktopUserAgentNeedsMigration(a.ClientIdentity) {
		t.Fatalf("migration changed device/profile or selected legacy UA: %+v", a.ClientIdentity)
	}
	stored, err := h.read(dir)
	if err != nil || *stored.ClientIdentity != *a.ClientIdentity {
		t.Fatalf("migration not durable: %v", err)
	}
	identity := *a.ClientIdentity
	if err := h.ensureClientIdentity(dir, a); err != nil || *a.ClientIdentity != identity {
		t.Fatalf("compatible identity rotated on replay: %v", err)
	}
}

func TestDesktopUAMigrationAvoidsUnconfirmedRemovalReservation(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	reserved, reservedDir := uaState(t, h, root, 1)
	reserved.ClientIdentity.UserAgentTemplate = "desktop-v1/0.164.0/Windows/10.0.22631/x86_64"
	reserved.ClientIdentity.UserAgent = desktopUserAgents()[reserved.ClientIdentity.UserAgentTemplate]
	reserved.DeleteRequested = true
	if err := h.save(reservedDir, reserved); err != nil {
		t.Fatal(err)
	}
	a, dir := oldDesktopUAState(t, h, root, 2, "0.150.0", "Windows", "10.0.22631", "x86_64")
	device := a.ClientIdentity.DeviceID
	if err := h.ensureClientIdentity(dir, a); err != nil {
		t.Fatal(err)
	}
	want := "desktop-v1/0.163.0/Windows/10.0.22631/x86_64"
	if a.ClientIdentity.DeviceID != device || a.ClientIdentity.UserAgentTemplate != want {
		t.Fatalf("unconfirmed removal reservation reused or desktop profile changed: %+v", a.ClientIdentity)
	}
}

func TestDesktopUAMigrationReusesConfirmedRemoval(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	reserved, reservedDir := uaState(t, h, root, 1)
	reserved.ClientIdentity.UserAgentTemplate = "desktop-v1/0.164.0/Windows/10.0.22631/x86_64"
	reserved.ClientIdentity.UserAgent = desktopUserAgents()[reserved.ClientIdentity.UserAgentTemplate]
	reserved.DeleteRequested, reserved.Phase, reserved.ContainerID = true, "removed", ""
	if err := h.save(reservedDir, reserved); err != nil {
		t.Fatal(err)
	}
	a, dir := oldDesktopUAState(t, h, root, 2, "0.150.0", "Windows", "10.0.22631", "x86_64")
	device := a.ClientIdentity.DeviceID
	if err := h.ensureClientIdentity(dir, a); err != nil {
		t.Fatal(err)
	}
	if a.ClientIdentity.DeviceID != device || a.ClientIdentity.UserAgent != reserved.ClientIdentity.UserAgent {
		t.Fatalf("migration did not reuse released UA or changed device: %+v", a.ClientIdentity)
	}
}

func TestDesktopUAMigrationSaveFailureRestoresIdentity(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	a, dir := oldDesktopUAState(t, h, root, 1, "0.150.0", "Mac OS", "14.0", "arm64")
	before, updatedAt := *a.ClientIdentity, a.UpdatedAt
	manifest := filepath.Join(dir, "manifest.json")
	saved := filepath.Join(dir, "manifest.saved")
	if err := os.Rename(manifest, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(saved, manifest); err != nil {
		t.Fatal(err)
	}
	if err := h.ensureClientIdentity(dir, a); err == nil {
		t.Fatal("migration succeeded when manifest persistence was forbidden")
	}
	if *a.ClientIdentity != before || !a.UpdatedAt.Equal(updatedAt) {
		t.Fatal("failed migration changed the in-memory identity")
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, manifest); err != nil {
		t.Fatal(err)
	}
	stored, err := h.read(dir)
	if err != nil || *stored.ClientIdentity != before {
		t.Fatalf("failed migration changed the saved identity: %v", err)
	}
}

func TestDesktopUAConcurrentLegacyMigrationsRemainUnique(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	versions := []string{"0.150.0", "0.155.0", "0.156.0", "0.157.0", "0.158.0"}
	states := make([]*State, len(versions))
	dirs := make([]string, len(versions))
	for i, version := range versions {
		states[i], dirs[i] = oldDesktopUAState(t, h, root, int64(i+1), version, "Mac OS", "14.0", "arm64")
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(states))
	for i := range states {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs <- h.ensureClientIdentity(dirs[i], states[i]) }(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, s := range states {
		if DesktopUserAgentNeedsMigration(s.ClientIdentity) || seen[s.ClientIdentity.UserAgent] {
			t.Fatalf("migration selected duplicate or legacy UA: %+v", s.ClientIdentity)
		}
		seen[s.ClientIdentity.UserAgent] = true
	}
}
