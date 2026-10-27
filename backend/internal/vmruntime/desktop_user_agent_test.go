package vmruntime

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func uaState(t *testing.T, h *Helper, root string, id int64) (*State, string) {
	t.Helper()
	identity, err := newIdentity()
	if err != nil {
		t.Fatal(err)
	}
	s := &State{AccountID: id, InstanceID: uuid.NewString(), Identity: identity, Spec: Spec{Platform: "codex"}, ClientIdentity: &ClientIdentity{DeviceID: uuid.NewString()}}
	dir := filepath.Join(root, s.InstanceID)
	if err := h.instanceDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := h.save(dir, s); err != nil {
		t.Fatal(err)
	}
	return s, dir
}
func TestDesktopUACatalogDesktopOnlyUniqueAndLargeEnough(t *testing.T) {
	catalog := desktopUserAgents()
	if len(catalog) < 999 {
		t.Fatalf("need at least 999 independent UAs, got %d", len(catalog))
	}
	seen := map[string]bool{}
	for id, ua := range catalog {
		if seen[ua] || (!strings.Contains(ua, "(Mac OS ") && !strings.Contains(ua, "(Windows ")) || strings.Contains(ua, "sub2") || strings.Contains(ua, "sub3") || strings.Contains(ua, "linux") {
			t.Fatalf("duplicate or invalid desktop profile %q", id)
		}
		seen[ua] = true
	}
	// Expansion must never invalidate previously assigned identities.
	for _, client := range []string{"0.164.0", "0.163.0", "0.162.1", "0.162.0"} {
		id := "desktop-v1/" + client + "/Mac OS/15.1/arm64"
		if catalog[id] != "codex_cli_rs/"+client+" (Mac OS 15.1; arm64)" {
			t.Fatalf("legacy profile changed: %s", id)
		}
	}
}

func TestDesktopUAConcurrentUniqueDurableAndExhausted(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	count := len(desktopUserAgents())
	states := make([]*State, count)
	dirs := make([]string, count)
	for i := range states {
		states[i], dirs[i] = uaState(t, h, root, int64(i+1))
	}
	var wg sync.WaitGroup
	errs := make(chan error, count)
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
	for i, s := range states {
		ua := s.ClientIdentity.UserAgent
		if ua == "" || seen[ua] {
			t.Fatal("duplicate or missing UA")
		}
		seen[ua] = true
		stored, err := h.read(dirs[i])
		if err != nil || *stored.ClientIdentity != *s.ClientIdentity {
			t.Fatal("not durable", err)
		}
	}
	extra, dir := uaState(t, h, root, int64(count+1))
	if got := errorCode(h.ensureClientIdentity(dir, extra)); got != "client_user_agent_pool_exhausted" {
		t.Fatal(got)
	}
	before := *states[0].ClientIdentity
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenHelper(root, &fakeDriver{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.ensureClientIdentity(dirs[0], states[0]); err != nil {
		t.Fatal(err)
	}
	if before != *states[0].ClientIdentity {
		t.Fatal("identity rotated")
	}
	states[0].ClientIdentity.UserAgent = "forged"
	if got := errorCode(reopened.ensureClientIdentity(dirs[0], states[0])); got != "client_user_agent_invalid" {
		t.Fatal(got)
	}
}
func TestDesktopUALegacyBackfillAndConflict(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	a, ad := uaState(t, h, root, 1)
	a.ContainerID = "existing"
	a.DesiredCredentialRevision = "c1"
	device := a.ClientIdentity.DeviceID
	if err := h.ensureClientIdentity(ad, a); err != nil {
		t.Fatal(err)
	}
	if a.ClientIdentity.DeviceID != device {
		t.Fatal("legacy device changed")
	}
	b, bd := uaState(t, h, root, 2)
	*b.ClientIdentity = *a.ClientIdentity
	if err := h.ensureClientIdentity(bd, b); errorCode(err) != "client_user_agent_conflict" {
		t.Fatal(err)
	}
}

func TestDesktopUAConfirmedRemovalReturnsReservation(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	a, ad := uaState(t, h, root, 1)
	if err := h.ensureClientIdentity(ad, a); err != nil {
		t.Fatal(err)
	}
	a.DeleteRequested = true
	a.Phase = "removed"
	a.ContainerID = ""
	if err := h.save(ad, a); err != nil {
		t.Fatal(err)
	}
	b, bd := uaState(t, h, root, 2)
	if err := h.ensureClientIdentity(bd, b); err != nil {
		t.Fatal(err)
	}
	if a.ClientIdentity.UserAgent != b.ClientIdentity.UserAgent || a.ClientIdentity.DeviceID == b.ClientIdentity.DeviceID {
		t.Fatal("confirmed removal did not return the UA to the pool")
	}
	before := *b.ClientIdentity
	if err := h.ensureClientIdentity(bd, b); err != nil || *b.ClientIdentity != before {
		t.Fatalf("reused UA was not stable for the new container: %v", err)
	}
	c, cd := uaState(t, h, root, 3)
	if err := h.ensureClientIdentity(cd, c); err != nil || c.ClientIdentity.UserAgent == b.ClientIdentity.UserAgent {
		t.Fatalf("reused UA was assigned to two live containers: %v", err)
	}
}

func TestDesktopUAUnconfirmedRemovalKeepsReservation(t *testing.T) {
	for _, tc := range []struct {
		name, phase, containerID string
		deleteRequested          bool
	}{
		{name: "delete_not_requested", phase: "removed"},
		{name: "delete_pending", phase: "remove", deleteRequested: true},
		{name: "delete_failed", phase: "error", deleteRequested: true},
		{name: "unknown_phase", phase: "unexpected", deleteRequested: true},
		{name: "empty_phase", deleteRequested: true},
		{name: "container_remains", phase: "removed", containerID: "existing", deleteRequested: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, root := openTest(t, &fakeDriver{})
			a, ad := uaState(t, h, root, 1)
			if err := h.ensureClientIdentity(ad, a); err != nil {
				t.Fatal(err)
			}
			a.DeleteRequested, a.Phase, a.ContainerID = tc.deleteRequested, tc.phase, tc.containerID
			if err := h.save(ad, a); err != nil {
				t.Fatal(err)
			}
			b, bd := uaState(t, h, root, 2)
			if err := h.ensureClientIdentity(bd, b); err != nil {
				t.Fatal(err)
			}
			if a.ClientIdentity.UserAgent == b.ClientIdentity.UserAgent {
				t.Fatalf("unconfirmed removal released UA: %+v", tc)
			}
		})
	}
}

func TestDesktopUAConcurrentAllocationAfterConfirmedRemoval(t *testing.T) {
	h, root := openTest(t, &fakeDriver{})
	a, ad := uaState(t, h, root, 1)
	if err := h.ensureClientIdentity(ad, a); err != nil {
		t.Fatal(err)
	}
	a.DeleteRequested, a.Phase, a.ContainerID = true, "removed", ""
	if err := h.save(ad, a); err != nil {
		t.Fatal(err)
	}
	b, bd := uaState(t, h, root, 2)
	c, cd := uaState(t, h, root, 3)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, item := range []struct {
		state *State
		dir   string
	}{{b, bd}, {c, cd}} {
		wg.Add(1)
		go func(state *State, dir string) {
			defer wg.Done()
			errs <- h.ensureClientIdentity(dir, state)
		}(item.state, item.dir)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if b.ClientIdentity.UserAgent == c.ClientIdentity.UserAgent {
		t.Fatal("concurrent allocations returned duplicate UAs")
	}
	if (b.ClientIdentity.UserAgent == a.ClientIdentity.UserAgent) == (c.ClientIdentity.UserAgent == a.ClientIdentity.UserAgent) {
		t.Fatal("exactly one concurrent allocation must receive the released UA")
	}
}
