package service

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const borrowEncryptedOutput = `[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque-observed-reasoning"},{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"21 with shape selection; 29 without"}]}]`

func borrowEncryptedHistoryFixture(t *testing.T) (*OpenAIGatewayService, *Account) {
	t.Helper()
	svc, account := borrowReplayFixture(t)
	ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "", []byte(borrowReplayInitial))
	svc.observeBorrowHTTPReplayResponse(ctx, account, []byte(`{"id":"resp_encrypted","status":"completed","output":`+borrowEncryptedOutput+`}`), "")
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(t.Context(), 0, "resp_encrypted", account.ID, time.Hour))
	return svc, account
}

func TestBorrowHTTPEncryptedHistoryRestoresOnlyToObservedAccount(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			svc, account := borrowEncryptedHistoryFixture(t)
			input := `[{"role":"user","content":"next"}]`
			if full {
				input = `[{"role":"user","content":"hello"},` + strings.Trim(borrowEncryptedOutput, "[]") + `,{"role":"user","content":"next"}]`
			}
			body := []byte(`{"input":` + input + `}`)
			ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_encrypted", body)
			ctx, effective, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, "resp_encrypted", "gpt-6-astra", nil, body)
			require.NoError(t, err)
			require.True(t, converted)
			items := gjson.GetBytes(effective, "input").Array()
			require.Len(t, items, 4)
			require.Equal(t, "opaque-observed-reasoning", items[1].Get("encrypted_content").String())
			require.Equal(t, "21 with shape selection; 29 without", items[2].Get("content.0.text").String())
			require.True(t, borrowHTTPContinuationAllowsAccount(ctx, account.ID))
			require.False(t, borrowHTTPContinuationAllowsAccount(ctx, account.ID+1))
			require.False(t, borrowHTTPContinuationFromContext(ctx).ReplayReady)
			require.Equal(t, "account_bound_reasoning", borrowHTTPContinuationFromContext(ctx).ReplayRefusal)
			require.False(t, svc.borrowHTTPContinuationCanFallback(ctx, nil))
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(effective))).WithContext(ctx)
			moved, _, reason := svc.MoveBorrowHTTPContinuation(req, nil, effective)
			require.Nil(t, moved)
			require.Equal(t, "account_bound_reasoning", reason)

			// A later response without encrypted output must retain the earlier
			// account restriction and all earlier conversation items.
			svc.observeBorrowHTTPReplayResponse(ctx, account, []byte(`{"id":"resp_next","status":"completed","output":[{"role":"assistant","content":"next answer"}]}`), "")
			key := borrowHTTPReplayKey{user: 1, apiKey: 2, response: sha256.Sum256([]byte("resp_next"))}
			proof, found := svc.borrowHTTPReplay.get(key, time.Now())
			require.True(t, found)
			require.True(t, proof.accountBound)
			next := []byte(`{"input":"third turn"}`)
			nextCtx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_next", next)
			_, restored, err := svc.restoreBorrowHTTPHistory(nextCtx, nil, "resp_next", account.ID, next)
			require.NoError(t, err)
			require.Len(t, gjson.GetBytes(restored, "input").Array(), 6)
		})
	}
}

func TestBorrowHTTPEncryptedHistoryRejectsUntrustedOrForeignState(t *testing.T) {
	for _, scenario := range []string{"foreign_user", "foreign_key", "foreign_group", "foreign_account", "changed_reasoning", "new_reasoning", "without_previous"} {
		t.Run(scenario, func(t *testing.T) {
			svc, account := borrowEncryptedHistoryFixture(t)
			user, key, group := int64(1), int64(2), int64(0)
			previous := "resp_encrypted"
			input := `[{"role":"user","content":"hello"},` + strings.Trim(borrowEncryptedOutput, "[]") + `,{"role":"user","content":"next"}]`
			target := account.ID
			switch scenario {
			case "foreign_user":
				user++
			case "foreign_key":
				key++
			case "foreign_group":
				group++
			case "foreign_account":
				target++
			case "changed_reasoning":
				input = strings.ReplaceAll(input, "opaque-observed-reasoning", "altered")
			case "new_reasoning":
				input = strings.TrimSuffix(input, "]") + `,{"type":"reasoning","encrypted_content":"new-opaque-state"}]`
			case "without_previous":
				previous = ""
			}
			body := []byte(`{"input":` + input + `}`)
			ctx := svc.PrepareBorrowHTTPReplay(t.Context(), &group, user, key, previous, body)
			_, _, err := svc.restoreBorrowHTTPHistory(ctx, &group, previous, target, body)
			require.ErrorIs(t, err, ErrBorrowHTTPHistoryUnavailable)
		})
	}
}

func TestBorrowHTTPEncryptedHistoryDoesNotTrustOtherOpaqueOutputs(t *testing.T) {
	for _, output := range []string{
		`{"type":"compaction","encrypted_content":"opaque"}`,
		`{"type":"reasoning","encrypted_content":"opaque","summary":[{"type":"item_reference","id":"private"}]}`,
		`{"type":"reasoning","encrypted_content":"opaque","file_id":"private"}`,
		`{"type":"message","encrypted_content":"opaque"}`,
	} {
		svc, account := borrowReplayFixture(t)
		ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "", []byte(borrowReplayInitial))
		svc.observeBorrowHTTPReplayResponse(ctx, account, []byte(`{"id":"resp_opaque","status":"completed","output":[`+output+`]}`), "")
		next := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_opaque", []byte(`{"input":"next"}`))
		require.Equal(t, "replay_evidence_missing", borrowHTTPReplayFromContext(next).reason)
	}
}
