package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const borrowReplayInitial = `{"model":"gpt-6-astra","input":[{"role":"user","content":"hello"}]}`
const borrowReplayCompleted = `{"type":"response.completed","response":{"id":"resp_proven","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}]}}`
const borrowReplayFull = `{"model":"gpt-6-astra","previous_response_id":"resp_proven","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"OK"},{"role":"user","content":"next"}]}`

func borrowReplayFixture(t *testing.T) (*OpenAIGatewayService, *Account) {
	t.Helper()
	borrow, _ := borrowFixture()
	svc := &OpenAIGatewayService{accountRepo: borrow.owner.accountRepo, borrowRoutes: borrow}
	a, err := svc.accountRepo.GetByID(t.Context(), 11)
	require.NoError(t, err)
	ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "", []byte(borrowReplayInitial))
	svc.observeBorrowHTTPReplayResponse(ctx, a, []byte(borrowReplayCompleted), "response.completed")
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(t.Context(), 0, "resp_proven", a.ID, time.Hour))
	return svc, a
}

func TestBorrowHTTPReplayRequiresObservedCompleteHistory(t *testing.T) {
	for _, tc := range []struct {
		name, body, previous, reason string
		user, key, group             int64
	}{
		{"full", borrowReplayFull, "resp_proven", "", 1, 2, 0},
		{"incremental", `{"input":"next"}`, "resp_proven", "incomplete_history", 1, 2, 0},
		{"altered history", strings.Replace(borrowReplayFull, "hello", "changed", 1), "resp_proven", "history_mismatch", 1, 2, 0},
		{"unobserved", borrowReplayFull, "resp_other", "replay_evidence_missing", 1, 2, 0},
		{"foreign user", borrowReplayFull, "resp_proven", "replay_evidence_missing", 3, 2, 0},
		{"foreign key", borrowReplayFull, "resp_proven", "replay_evidence_missing", 1, 3, 0},
		{"foreign group", borrowReplayFull, "resp_proven", "replay_evidence_missing", 1, 2, 3},
		{"encrypted", `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, "resp_proven", "nonportable_context", 1, 2, 0},
		{"item reference", `{"input":[{"type":"item_reference","id":"msg_1"}]}`, "resp_proven", "nonportable_context", 1, 2, 0},
		{"conversation", `{"conversation":"conv_1","input":"next"}`, "resp_proven", "nonportable_context", 1, 2, 0},
		{"file resource", `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_private"}]}]}`, "resp_proven", "nonportable_context", 1, 2, 0},
		{"vector store", `{"input":"next","tools":[{"type":"file_search","vector_store_ids":["vs_private"]}]}`, "resp_proven", "nonportable_context", 1, 2, 0},
		{"orphan output", `{"input":[{"type":"function_call_output","call_id":"call_unknown","output":"result"}]}`, "resp_proven", "incomplete_tool_context", 1, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := borrowReplayFixture(t)
			ctx := svc.PrepareBorrowHTTPReplay(t.Context(), &tc.group, tc.user, tc.key, tc.previous, []byte(tc.body))
			require.Equal(t, tc.reason, borrowHTTPReplayFromContext(ctx).reason)
		})
	}
}

func TestBorrowHTTPReplayTerminalProofAndLimits(t *testing.T) {
	for _, status := range []string{"failed", "incomplete", "cancelled", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			svc, a := borrowReplayFixture(t)
			ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "", []byte(borrowReplayInitial))
			payload := strings.ReplaceAll(strings.Replace(borrowReplayCompleted, "resp_proven", "resp_rejected", 1), "completed", status)
			svc.observeBorrowHTTPReplayResponse(ctx, a, []byte(payload), "response."+status)
			next := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_rejected", []byte(borrowReplayFull))
			require.Equal(t, "replay_evidence_missing", borrowHTTPReplayFromContext(next).reason)
		})
	}
	svc, _ := borrowReplayFixture(t)
	key := borrowHTTPReplayKey{user: 1, apiKey: 2, response: sha256.Sum256([]byte("resp_proven"))}
	svc.borrowHTTPReplay.mu.Lock()
	proof := svc.borrowHTTPReplay.entries[key]
	proof.expires = time.Now().Add(-time.Second)
	svc.borrowHTTPReplay.entries[key] = proof
	svc.borrowHTTPReplay.mu.Unlock()
	ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_proven", []byte(borrowReplayFull))
	require.Equal(t, "replay_evidence_missing", borrowHTTPReplayFromContext(ctx).reason)
	require.Empty(t, svc.borrowHTTPReplay.entries)
	for i := 0; i < borrowHTTPReplayMaxEntries+10; i++ {
		key.response = sha256.Sum256([]byte(fmt.Sprint(i)))
		svc.borrowHTTPReplay.put(key, borrowHTTPReplayProof{account: 11, expires: time.Now().Add(time.Minute)})
	}
	require.Len(t, svc.borrowHTTPReplay.entries, borrowHTTPReplayMaxEntries)
}

func TestBorrowHTTPReplayVerifiesActualToolCallHistory(t *testing.T) {
	svc, a := borrowReplayFixture(t)
	ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "", []byte(borrowReplayInitial))
	svc.observeBorrowHTTPReplayResponse(ctx, a, []byte(`{"type":"response.completed","response":{"id":"resp_tool","status":"completed","output":[{"type":"function_call","id":"fc_1","status":"completed","call_id":"call_1","name":"lookup","arguments":"{}"}]}}`), "response.completed")
	body := []byte(`{"input":[{"role":"user","content":"hello"},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"actual result"}]}`)
	ctx = svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_tool", body)
	require.Empty(t, borrowHTTPReplayFromContext(ctx).reason)
	changed := strings.Replace(string(body), `"name":"lookup"`, `"name":"fabricated"`, 1)
	ctx = svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_tool", []byte(changed))
	require.Equal(t, "history_mismatch", borrowHTTPReplayFromContext(ctx).reason, "pairing call IDs alone cannot authenticate the history")
}

func TestBorrowHTTPReplayConcurrentRequestsDoNotShareMutableState(t *testing.T) {
	svc, a := borrowReplayFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_proven", []byte(borrowReplayFull))
			require.Empty(t, borrowHTTPReplayFromContext(ctx).reason)
			payload := strings.Replace(borrowReplayCompleted, "resp_proven", fmt.Sprintf("resp_parallel_%d", i), 1)
			svc.observeBorrowHTTPReplayResponse(ctx, a, []byte(payload), "response.completed")
		}(i)
	}
	wg.Wait()
	require.Len(t, svc.borrowHTTPReplay.entries, 41)
}

func TestBorrowHTTPContinuationMoveClearsOnlyRequestBindings(t *testing.T) {
	svc, _ := borrowReplayFixture(t)
	ctx, recovery := WithBorrowBusinessRecovery(t.Context(), nil)
	defer recovery.Close()
	ctx = svc.PrepareBorrowHTTPReplay(ctx, nil, 1, 2, "resp_proven", []byte(borrowReplayFull))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(borrowReplayFull)).WithContext(ctx)
	for _, key := range []string{"session_id", "conversation_id", "thread_id", "X-Codex-Thread-ID", "X-Codex-Turn-State", "X-Codex-Turn-Metadata"} {
		req.Header.Set(key, "old")
	}
	ctx, body, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, "resp_proven", "gpt-6-astra", req.Header, []byte(borrowReplayFull))
	require.NoError(t, err)
	require.True(t, converted)
	ctx = WithBorrowBusinessBinding(ctx, req.Header, "resp_proven")
	req = req.WithContext(ctx)
	require.NoError(t, recovery.Activate(11, true))
	deadline := time.Now().Add(3 * time.Second)
	recovery.routeWaitDeadline = deadline
	moved, effective, reason := svc.MoveBorrowHTTPContinuation(req, nil, body)
	require.Empty(t, reason)
	require.NotNil(t, moved)
	require.Nil(t, borrowHTTPContinuationFromContext(moved.Context()))
	require.NotNil(t, borrowHTTPContinuationFromContext(req.Context()))
	require.True(t, OpenAIInitialAdmissionCanReselect(moved.Context(), effective))
	require.False(t, recovery.Pinned())
	require.Equal(t, deadline, recovery.routeWaitDeadline)
	require.Equal(t, "old", req.Header.Get("session_id"))
	require.NotEmpty(t, moved.Header.Get("session_id"))
	require.NotEqual(t, "old", moved.Header.Get("session_id"))
	for _, key := range []string{"thread_id", "X-Codex-Thread-ID", "X-Codex-Turn-State", "X-Codex-Turn-Metadata"} {
		require.Empty(t, moved.Header.Get(key))
	}
	reader, err := moved.GetBody()
	require.NoError(t, err)
	copy, err := io.ReadAll(reader)
	require.NoError(t, err)
	reader.Close()
	require.Equal(t, effective, copy)
	require.EqualValues(t, len(effective), moved.ContentLength)
	require.False(t, gjson.GetBytes(effective, "previous_response_id").Exists())
	id, err := svc.getOpenAIWSStateStore().GetResponseAccount(t.Context(), 0, "resp_proven")
	require.NoError(t, err)
	require.EqualValues(t, 11, id, "historical global ownership remains usable by concurrent requests")
	second, _, _ := svc.MoveBorrowHTTPContinuation(moved, nil, effective)
	require.Nil(t, second, "one conversion per request")
}

func TestBorrowHTTPContinuationCannotMoveAfterTransportOrCancellation(t *testing.T) {
	for _, boundary := range []string{"transport", "write", "output", "cancel", "closed", "recovery", "foreign_pin"} {
		t.Run(boundary, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, r := WithBorrowBusinessRecovery(ctx, nil)
			defer r.Close()
			require.NoError(t, r.Activate(11, true))
			switch boundary {
			case "transport":
				require.NoError(t, r.BeginTransport(ctx))
			case "write":
				require.NoError(t, r.WritePossible())
			case "output":
				r.OutputStarted(ctx)
			case "cancel":
				cancel()
			case "closed":
				r.Close()
			case "recovery":
				r.recovering = true
			case "foreign_pin":
				r.pinnedTarget = 12
			}
			require.False(t, r.httpContinuationUnsent(11, true))
			require.NotZero(t, r.pinnedTarget)
		})
	}
}
