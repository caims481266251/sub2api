package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBorrowHTTPHistoryRestoreAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		wantError   bool
		count       int
	}{
		{"delta_string", `"next"`, false, 3},
		{"delta_messages", `[{"role":"user","content":"next"},{"role":"user","content":"and another"}]`, false, 4},
		{"full_history", gjson.Get(borrowReplayFull, "input").Raw, false, 3},
		{"altered_history", `[{"role":"user","content":"changed"},{"role":"assistant","content":"OK"}]`, true, 0},
		{"orphan_tool", `[{"type":"function_call_output","call_id":"missing","output":"value"}]`, true, 0},
		{"encrypted", `[{"type":"reasoning","encrypted_content":"opaque"}]`, true, 0},
		{"file", `[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]`, true, 0},
		{"too_large", fmt.Sprintf(`%q`, strings.Repeat("x", borrowHTTPReplayMaxBytes)), true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := borrowReplayFixture(t)
			body := []byte(`{"previous_response_id":"resp_proven","input":` + tc.input + `}`)
			original := string(body)
			ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_proven", body)
			ctx, effective, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, "resp_proven", "gpt-6-astra", http.Header{}, body)
			if tc.wantError {
				require.ErrorIs(t, err, ErrBorrowHTTPHistoryUnavailable)
				require.False(t, converted)
				return
			}
			require.NoError(t, err)
			require.True(t, converted)
			require.Len(t, gjson.GetBytes(effective, "input").Array(), tc.count)
			require.Equal(t, original, string(body), "audit input stays immutable")
			require.True(t, borrowHTTPContinuationFromContext(ctx).ReplayReady)
			require.Empty(t, borrowHTTPReplayFromContext(ctx).reason)
			if tc.name == "full_history" {
				require.JSONEq(t, tc.input, gjson.GetBytes(effective, "input").Raw, "complete input must not be duplicated")
			}
		})
	}
}

func TestBorrowHTTPHistoryRestoresToolCallForDeltaOutput(t *testing.T) {
	svc, account := borrowReplayFixture(t)
	body := []byte(borrowReplayInitial)
	ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "", body)
	svc.observeBorrowHTTPReplayResponse(ctx, account, []byte(`{"id":"resp_tool","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"key\":\"original\"}"}]}`), "")
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(t.Context(), 0, "resp_tool", account.ID, time.Hour))
	body = []byte(`{"previous_response_id":"resp_tool","input":[{"type":"function_call_output","call_id":"call_1","output":"result"}]}`)
	ctx = svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_tool", body)
	ctx, effective, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, "resp_tool", "gpt-6-astra", nil, body)
	require.NoError(t, err)
	require.True(t, converted)
	items := gjson.GetBytes(effective, "input").Array()
	require.Len(t, items, 3)
	require.Equal(t, "lookup", items[1].Get("name").String())
	require.JSONEq(t, `{"key":"original"}`, items[1].Get("arguments").String())
	require.Equal(t, items[1].Get("call_id").String(), items[2].Get("call_id").String())
	require.True(t, borrowHTTPContinuationFromContext(ctx).ReplayReady)
}

func TestBorrowHTTPHistoryMissingExpiredForeignOrFailedTerminalIsRejected(t *testing.T) {
	for _, scenario := range []string{"restart", "expired", "foreign_user", "foreign_key", "foreign_group", "account_mismatch", "conflicting_response", "failed_terminal"} {
		t.Run(scenario, func(t *testing.T) {
			svc, account := borrowReplayFixture(t)
			key := borrowHTTPReplayKey{user: 1, apiKey: 2, response: sha256.Sum256([]byte("resp_proven"))}
			user, apiKey, group := int64(1), int64(2), int64(0)
			switch scenario {
			case "restart":
				svc.borrowHTTPReplay = borrowHTTPReplayStore{}
			case "expired":
				proof := svc.borrowHTTPReplay.entries[key]
				proof.expires = time.Now().Add(-time.Second)
				svc.borrowHTTPReplay.entries[key] = proof
			case "foreign_user":
				user++
			case "foreign_key":
				apiKey++
			case "foreign_group":
				group++
			case "account_mismatch":
				proof := svc.borrowHTTPReplay.entries[key]
				proof.account++
				svc.borrowHTTPReplay.entries[key] = proof
			case "conflicting_response":
				svc.borrowHTTPReplay.put(key, borrowHTTPReplayProof{account: 12, count: 2, expires: time.Now().Add(time.Minute)})
			case "failed_terminal":
				svc.borrowHTTPReplay = borrowHTTPReplayStore{}
				ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "", []byte(borrowReplayInitial))
				svc.observeBorrowHTTPReplayResponse(ctx, account, []byte(strings.ReplaceAll(borrowReplayCompleted, "completed", "failed")), "response.failed")
			}
			body := []byte(`{"input":"next"}`)
			ctx := svc.PrepareBorrowHTTPReplay(t.Context(), &group, user, apiKey, "resp_proven", body)
			_, _, err := svc.restoreBorrowHTTPHistory(ctx, &group, "resp_proven", 11, body)
			require.ErrorIs(t, err, ErrBorrowHTTPHistoryUnavailable)
		})
	}
}

func TestBorrowHTTPHistoryByteBudgetAndImmutableSnapshots(t *testing.T) {
	var store borrowHTTPReplayStore
	history := json.RawMessage(strings.Repeat("x", borrowHTTPReplayMaxBytes))
	for i := 0; i < 20; i++ {
		key := borrowHTTPReplayKey{response: sha256.Sum256([]byte(fmt.Sprint(i)))}
		store.put(key, borrowHTTPReplayProof{account: 11, count: 1, history: append(json.RawMessage(nil), history...), expires: time.Now().Add(time.Minute)})
	}
	require.Equal(t, borrowHTTPHistoryMaxBytes, store.bytes)
	require.Len(t, store.entries, borrowHTTPHistoryMaxBytes/borrowHTTPReplayMaxBytes)
	for key, proof := range store.entries {
		store.put(key, borrowHTTPReplayProof{account: 12, expires: proof.expires})
	}
	require.Zero(t, store.bytes, "conflicts drop their snapshots")
	svc, _ := borrowReplayFixture(t)
	key := borrowHTTPReplayKey{user: 1, apiKey: 2, response: sha256.Sum256([]byte("resp_proven"))}
	before, _ := svc.borrowHTTPReplay.get(key, time.Now())
	for _, input := range []string{`"branch A"`, `"branch B"`} {
		body := []byte(`{"input":` + input + `}`)
		ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_proven", body)
		_, _, err := svc.restoreBorrowHTTPHistory(ctx, nil, "resp_proven", 11, body)
		require.NoError(t, err)
	}
	after, _ := svc.borrowHTTPReplay.get(key, time.Now())
	require.Equal(t, before.history, after.history)
}

func TestBorrowHTTPHistoryConcurrentBranches(t *testing.T) {
	svc, account := borrowReplayFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(fmt.Sprintf(`{"input":"branch %d"}`, i))
			ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_proven", body)
			ctx, effective, err := svc.restoreBorrowHTTPHistory(ctx, nil, "resp_proven", 11, body)
			require.NoError(t, err)
			require.Len(t, gjson.GetBytes(effective, "input").Array(), 3)
			require.Equal(t, fmt.Sprintf("branch %d", i), gjson.GetBytes(effective, "input.2.content").String())
			payload := strings.Replace(borrowReplayCompleted, "resp_proven", fmt.Sprintf("resp_branch_%d", i), 1)
			svc.observeBorrowHTTPReplayResponse(ctx, account, []byte(payload), "response.completed")
		}(i)
	}
	wg.Wait()
	require.Len(t, svc.borrowHTTPReplay.entries, 33)
}
