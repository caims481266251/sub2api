package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var ErrBorrowHTTPHistoryUnavailable = errors.New("borrowed HTTP continuation context is unavailable or incomplete; send the complete conversation in input without previous_response_id")

// Owner validation precedes this call. Only the authenticated user/key/group
// snapshot may supply missing items; client-supplied assistant/tool history must
// match the existing proof instead of being mistaken for a new delta.
func (s *OpenAIGatewayService) restoreBorrowHTTPHistory(ctx context.Context, groupID *int64, previous string, accountID int64, body []byte) (context.Context, []byte, error) {
	r := borrowHTTPReplayFromContext(ctx)
	if r == nil || r.key.group != derefGroupID(groupID) {
		return ctx, body, ErrBorrowHTTPHistoryUnavailable
	}
	if r.reason != "" || r.previousAccount != accountID {
		key := r.key
		key.response = sha256.Sum256([]byte(previous))
		proof, found := s.borrowHTTPReplay.get(key, time.Now())
		if !found || proof.account != accountID || len(proof.history) == 0 {
			return ctx, body, ErrBorrowHTTPHistoryUnavailable
		}
		items, exists, err := openAIWSExtractNormalizedInputSequence(body)
		if err != nil || !exists || len(items) == 0 || len(body) > borrowHTTPReplayMaxBytes ||
			proof.count+len(items) > borrowHTTPReplayMaxItems || !borrowHTTPHistoryDelta(items) {
			return ctx, body, ErrBorrowHTTPHistoryUnavailable
		}
		var history []json.RawMessage
		if json.Unmarshal(proof.history, &history) != nil {
			return ctx, body, ErrBorrowHTTPHistoryUnavailable
		}
		body, err = sjson.SetBytes(body, "input", normalizeBorrowHTTPHistoryStrings(append(history, items...)))
		if err != nil {
			return ctx, body, ErrBorrowHTTPHistoryUnavailable
		}
	}
	// Recheck the actual forwarding body, including paired tool outputs and
	// portability, then establish evidence for the next completed response.
	ctx = s.PrepareBorrowHTTPReplay(ctx, groupID, r.key.user, r.key.apiKey, previous, body)
	r = borrowHTTPReplayFromContext(ctx)
	if r.reason != "" || r.previousAccount != accountID {
		return ctx, body, ErrBorrowHTTPHistoryUnavailable
	}
	return ctx, body, nil
}

func borrowHTTPHistoryDelta(items []json.RawMessage) bool {
	for _, item := range items {
		v := gjson.ParseBytes(item)
		if v.Type == gjson.String {
			continue
		}
		if isCodexToolCallOutputItemType(v.Get("type").String()) {
			continue // validate against restored call IDs after merging
		}
		if typ := v.Get("type").String(); typ != "" && typ != "message" {
			return false
		}
		switch v.Get("role").String() {
		case "user", "system", "developer":
		default:
			return false
		}
	}
	return true
}

// The Responses string shorthand is a user message, but array items must be
// objects. Keep the semantic digest unchanged while producing valid wire input.
func normalizeBorrowHTTPHistoryStrings(items []json.RawMessage) []json.RawMessage {
	for i, item := range items {
		if v := gjson.ParseBytes(item); v.Type == gjson.String {
			items[i], _ = json.Marshal(map[string]string{"type": "message", "role": "user", "content": v.String()})
		}
	}
	return items
}
