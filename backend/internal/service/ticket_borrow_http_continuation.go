package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This context is created only after public HTTP response ownership validation.
// The response key remains internal: Codex HTTP does not accept previous_response_id.
type borrowHTTPContinuation struct {
	TargetID                        int64
	Response, Session, FreshSession string
	ReplayReady                     bool
	ReplayRefusal                   string
}
type borrowHTTPContinuationKey struct{}

func borrowHTTPContinuationFromContext(ctx context.Context) *borrowHTTPContinuation {
	c, _ := ctx.Value(borrowHTTPContinuationKey{}).(*borrowHTTPContinuation)
	return c
}

func borrowHTTPContinuationAllowsAccount(ctx context.Context, id int64) bool {
	c := borrowHTTPContinuationFromContext(ctx)
	return c == nil || c.TargetID == id
}

// BorrowContinuationRoute carries only routing metadata, never conversation history.
type BorrowContinuationRoute struct {
	Route     *BorrowRoute
	SessionID string
	Restarted bool
}

type BorrowHTTPContinuationRepository interface {
	CheckHTTPContinuation(context.Context, int64, string, string, string) error
	ReserveHTTPContinuation(context.Context, int64, string, string, string, string, string) (*BorrowContinuationRoute, error)
	BindHTTPContinuationResponse(context.Context, int64, string, string, string) error
}

// New/session-only business requests share latest-route selection with HTTP
// response continuations. Exact-route reads remain available for send fencing.
type BorrowLatestRouteRepository interface {
	BorrowHTTPContinuationRepository
	CheckLatestRoute(context.Context, int64, string, string) error
	ReserveLatestRoute(context.Context, int64, string, string, string, string) (*BorrowContinuationRoute, error)
}

// PrepareBorrowHTTPContinuation must be called after ValidateOpenAIHTTPResponseOwner.
// It does not reserve capacity or change the original client request/audit identity.
func (s *OpenAIGatewayService) PrepareBorrowHTTPContinuation(ctx context.Context, groupID *int64, previous, model string, headers http.Header, body []byte) (context.Context, []byte, bool, error) {
	if previous == "" || s.borrowRoutes == nil || strings.TrimSpace(model) == "" || borrowInternal(ctx) {
		return ctx, body, false, nil
	}
	id, err := s.getOpenAIWSStateStore().GetResponseAccount(ctx, derefGroupID(groupID), previous)
	if err != nil {
		return ctx, body, false, borrowAdmissionError(err)
	}
	if id <= 0 {
		return ctx, body, false, nil
	}
	a, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return ctx, body, false, borrowAdmissionError(err)
	}
	if a == nil || !a.IsOpenAI() || !a.UsesOpenAICodexProtocol() {
		return ctx, body, false, nil
	}
	enabled, err := s.borrowRoutes.Enabled(ctx, id)
	if err != nil {
		return ctx, body, false, borrowAdmissionError(err)
	}
	if !enabled {
		return ctx, body, false, nil
	}
	if err := validateBorrowHTTPContinuationInput(body); err != nil {
		return ctx, body, false, err
	}
	effective, err := sjson.DeleteBytes(body, "previous_response_id")
	if err != nil {
		return ctx, body, false, err
	}
	hint := &borrowHTTPContinuation{TargetID: id, Response: borrowResponseKey(previous), Session: borrowSessionKey(headers), FreshSession: uuid.NewString()}
	hint.ReplayRefusal = "replay_evidence_missing"
	if replay := borrowHTTPReplayFromContext(ctx); replay != nil {
		hint.ReplayRefusal = replay.reason
		if hint.ReplayRefusal == "" && replay.previousAccount != id {
			hint.ReplayRefusal = "replay_account_mismatch"
		}
		hint.ReplayReady = hint.ReplayRefusal == ""
	}
	return context.WithValue(ctx, borrowHTTPContinuationKey{}, hint), effective, true, nil
}

// Only this compatibility path requires self-contained input. Keep the normal
// API-key/WS validators and OAuth sanitizers unchanged.
func validateBorrowHTTPContinuationInput(body []byte) error {
	root := gjson.ParseBytes(body)
	if v := root.Get("conversation"); v.Exists() && v.Type != gjson.Null {
		return errors.New("borrowed HTTP continuation requires input context instead of conversation")
	}
	calls := map[string]bool{}
	for _, item := range root.Get("input").Array() {
		typ := item.Get("type").String()
		if typ == "item_reference" {
			return errors.New("borrowed HTTP continuation requires actual input items instead of item_reference")
		}
		if isCodexToolCallContextItemType(typ) {
			id := strings.TrimSpace(item.Get("call_id").String())
			if id == "" {
				id = strings.TrimSpace(item.Get("id").String())
			}
			if id != "" {
				calls[id] = true
			}
		}
	}
	for _, item := range root.Get("input").Array() {
		typ := item.Get("type").String()
		if isCodexToolCallOutputItemType(typ) && !calls[strings.TrimSpace(item.Get("call_id").String())] {
			return errors.New("borrowed HTTP continuation requires a matching tool call for every output call_id in input")
		}
	}
	return nil
}

// Applied to the final upstream clone after route injection. All client/audit
// state stays intact. Body and GetBody must describe the same effective request.
func applyBorrowHTTPContinuation(req *http.Request, choice *BorrowContinuationRoute) error {
	// A fresh request has no continuation state to translate. Preserve its body
	// and GetBody exactly as the ordinary gateway built them.
	if choice.SessionID == "" && !choice.Restarted && borrowHTTPContinuationFromContext(req.Context()) == nil {
		return nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(rc, (32<<20)+1))
	rc.Close()
	if err != nil {
		return err
	}
	if len(body) > 32<<20 || !gjson.ValidBytes(body) {
		return ErrBorrowUnavailable
	}
	body, err = sjson.DeleteBytes(body, "previous_response_id")
	if err != nil {
		return err
	}
	if choice.SessionID != "" {
		for _, key := range append(append(append([]string{}, explicitOpenAIHeaderSessionNames...), openAIThreadIdentityHeaders...), "X-Codex-Thread-ID") {
			req.Header.Del(key)
		}
		req.Header.Set("session_id", choice.SessionID)
		req.Header.Set("conversation_id", choice.SessionID)
		for _, path := range []string{"prompt_cache_key", "client_metadata.session_id", "client_metadata.thread_id"} {
			if path != "prompt_cache_key" && !gjson.GetBytes(body, path).Exists() {
				continue
			}
			body, err = sjson.SetBytes(body, path, choice.SessionID)
			if err != nil {
				return err
			}
		}
	}
	// Session-only requests can arrive concurrently with headers from before the
	// migration. Without a response binding, their turn-state cannot be attributed
	// to the new effective session. Keep its identity but discard that stale hint.
	if choice.Restarted || (choice.SessionID != "" && borrowHTTPContinuationFromContext(req.Context()) == nil) {
		req.Header.Del("X-Codex-Turn-State")
		req.Header.Del("X-Codex-Turn-Metadata")
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Length")
	return nil
}
