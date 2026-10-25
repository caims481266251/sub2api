package service

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/tidwall/sjson"
)

func BorrowHTTPContinuationStatus(ctx context.Context) (target int64, responseHash, refusal string) {
	if c := borrowHTTPContinuationFromContext(ctx); c != nil {
		return c.TargetID, c.Response, c.ReplayRefusal
	}
	return 0, "", ""
}

// Capacity selection must give the handler a chance to move a proven complete
// continuation before spending its shared route-wait budget on the old target.
func (s *OpenAIGatewayService) borrowHTTPContinuationCanFallback(ctx context.Context, groupID *int64) bool {
	c := borrowHTTPContinuationFromContext(ctx)
	replay := borrowHTTPReplayFromContext(ctx)
	_, taskBound := openAIGuardianParentAffinityFromContext(ctx)
	return c != nil && c.ReplayReady && ctx.Err() == nil &&
		replay != nil && replay.key.group == derefGroupID(groupID) && !taskBound &&
		BorrowBusinessRecoveryFromContext(ctx).httpContinuationUnsent(c.TargetID, false)
}

// The caller must prove selection failed before any forwarding, or supply an
// IsOpenAIInitialAdmissionRejection from the first account attempt, and enforce
// the handler's existing output/cancellation/account-switch budget checks.
// Only request-local bindings are replaced. Other continuations keep their
// original global response/session ownership.
func (s *OpenAIGatewayService) MoveBorrowHTTPContinuation(req *http.Request, groupID *int64, body []byte) (*http.Request, []byte, string) {
	ctx := req.Context()
	c := borrowHTTPContinuationFromContext(ctx)
	if c == nil {
		return nil, nil, "not_borrowed_continuation"
	}
	if !c.ReplayReady {
		return nil, nil, c.ReplayRefusal
	}
	if _, bound := openAIGuardianParentAffinityFromContext(ctx); bound {
		return nil, nil, "task_owner_bound"
	}
	if ctx.Err() != nil || !BorrowBusinessRecoveryFromContext(ctx).httpContinuationUnsent(c.TargetID, false) {
		return nil, nil, "request_not_unsent"
	}
	// Verify the actual forwarding input too; proof was prepared on the public
	// request before model mapping. Never open a path for opaque account state.
	items, reason := borrowHTTPReplayInput(body)
	if reason != "" {
		return nil, nil, reason
	}
	replay := borrowHTTPReplayFromContext(ctx)
	var digest [32]byte
	for _, item := range items {
		var ok bool
		digest, ok = appendBorrowReplayDigest(digest, item)
		if !ok {
			return nil, nil, "forward_input_changed"
		}
	}
	if replay == nil || replay.key.group != derefGroupID(groupID) || replay.digest != digest || replay.count != len(items) {
		return nil, nil, "forward_input_changed"
	}
	for _, path := range []string{"client_metadata.x-codex-turn-state", "client_metadata.x-codex-turn-metadata"} {
		var err error
		body, err = sjson.DeleteBytes(body, path)
		if err != nil {
			return nil, nil, "forward_rewrite_failed"
		}
	}
	forward := req.Clone(ctx)
	forward.Body = io.NopCloser(bytes.NewReader(body))
	forward.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	if err := applyBorrowHTTPContinuation(forward, &BorrowContinuationRoute{SessionID: c.FreshSession, Restarted: true}); err != nil {
		return nil, nil, "forward_rewrite_failed"
	}
	reader, err := forward.GetBody()
	if err != nil {
		return nil, nil, "forward_rewrite_failed"
	}
	effective, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		return nil, nil, "forward_rewrite_failed"
	}
	if !BorrowBusinessRecoveryFromContext(ctx).httpContinuationUnsent(c.TargetID, true) {
		return nil, nil, "request_not_unsent"
	}
	ctx = context.WithValue(ctx, borrowHTTPContinuationKey{}, (*borrowHTTPContinuation)(nil))
	ctx = WithBorrowBusinessBinding(ctx, forward.Header, "")
	return forward.WithContext(ctx), effective, ""
}

// Preserve attempts, deadlines, cancellation and send evidence. The migration
// cannot reset an exhausted/ambiguous recovery to manufacture another send.
func (r *BorrowBusinessRecovery) httpContinuationUnsent(target int64, release bool) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.err != nil || r.client.Err() != nil || r.committed || r.writePossible ||
		r.written || r.transports != 0 || r.sends != 0 || r.recovering ||
		(r.pinnedTarget != 0 && r.pinnedTarget != target) {
		return false
	}
	if release {
		r.pinnedTarget = 0
		r.fallback = 0
	}
	return true
}
