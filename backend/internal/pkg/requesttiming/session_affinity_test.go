package requesttiming

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSessionAffinitySnapshotIsFrozenAndSerialized(t *testing.T) {
	c := New(time.Now(), 0)
	ctx := With(t.Context(), c)
	state := SessionAffinitySnapshot{Decision: "temporary_spillover", Reason: "account_busy", OwnerAccountID: 11, ExecutionAccountID: 12, Success: true}
	SessionAffinity(ctx, state)
	var first, second Snapshot
	c.WhenFinished(func(s Snapshot) { first = s })
	c.Finish(200, false)
	state.OwnerAccountID = 99
	SessionAffinity(ctx, state)
	c.WhenFinished(func(s Snapshot) { second = s })
	if first.SessionAffinity == nil || first.SessionAffinity.OwnerAccountID != 11 {
		t.Fatal("late mutation changed snapshot")
	}
	first.SessionAffinity.OwnerAccountID = 88
	if second.SessionAffinity.OwnerAccountID != 11 {
		t.Fatal("snapshot pointer shared between readers")
	}
	raw, err := json.Marshal(second)
	if err != nil || !strings.Contains(string(raw), `"session_affinity"`) || !strings.Contains(string(raw), `"temporary_spillover"`) {
		t.Fatalf("missing persisted decision: %s, %v", raw, err)
	}
}
