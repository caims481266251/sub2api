package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	borrowHTTPReplayMaxBytes   = 4 << 20
	borrowHTTPReplayMaxItems   = 4096
	borrowHTTPReplayMaxEntries = 8192
	borrowHTTPReplayTTL        = 30 * time.Minute
)

// A bounded, process-local proof of history, NOT a conversation cache. A miss
// (including restart/another instance) leaves the original target restriction
// in place. Neither raw input/output nor credentials survive the request.
type borrowHTTPReplayStore struct {
	mu      sync.Mutex
	entries map[borrowHTTPReplayKey]borrowHTTPReplayProof
}

type borrowHTTPReplayKey struct {
	group, user, apiKey int64
	response            [32]byte
}

type borrowHTTPReplayProof struct {
	digest  [32]byte
	count   int
	account int64
	expires time.Time
}

type borrowHTTPReplayRequest struct {
	key             borrowHTTPReplayKey
	digest          [32]byte
	count           int
	previousAccount int64
	reason          string
}

type borrowHTTPReplayContextKey struct{}

func borrowHTTPReplayFromContext(ctx context.Context) *borrowHTTPReplayRequest {
	r, _ := ctx.Value(borrowHTTPReplayContextKey{}).(*borrowHTTPReplayRequest)
	return r
}

func (s *borrowHTTPReplayStore) get(key borrowHTTPReplayKey, now time.Time) (borrowHTTPReplayProof, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.entries[key]
	if ok && !now.Before(p.expires) {
		delete(s.entries, key)
		ok = false
	}
	return p, ok
}

func (s *borrowHTTPReplayStore) put(key borrowHTTPReplayKey, proof borrowHTTPReplayProof) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[borrowHTTPReplayKey]borrowHTTPReplayProof)
	}
	// Immutable response history. Conflicting observations fail closed.
	if old, ok := s.entries[key]; ok {
		if old.digest != proof.digest || old.count != proof.count || old.account != proof.account {
			s.entries[key] = borrowHTTPReplayProof{expires: proof.expires}
		}
		return
	}
	if len(s.entries) >= borrowHTTPReplayMaxEntries {
		for oldKey := range s.entries {
			delete(s.entries, oldKey)
			break
		}
	}
	s.entries[key] = proof
}

// Called only by authenticated Responses HTTP ingress after owner validation.
// Complete history is established by an exact semantic prefix of a request and
// completed response this process actually observed, never by input shape/size.
func (s *OpenAIGatewayService) PrepareBorrowHTTPReplay(ctx context.Context, groupID *int64, userID, apiKeyID int64, previous string, body []byte) context.Context {
	if s.borrowRoutes == nil || userID <= 0 || apiKeyID <= 0 || borrowInternal(ctx) {
		return ctx
	}
	r := &borrowHTTPReplayRequest{key: borrowHTTPReplayKey{group: derefGroupID(groupID), user: userID, apiKey: apiKeyID}}
	ctx = context.WithValue(ctx, borrowHTTPReplayContextKey{}, r)
	items, reason := borrowHTTPReplayInput(body)
	if reason != "" {
		r.reason = reason
		return ctx
	}
	var previousProof borrowHTTPReplayProof
	if previous != "" {
		key := r.key
		key.response = sha256.Sum256([]byte(previous))
		var found bool
		previousProof, found = s.borrowHTTPReplay.get(key, time.Now())
		if !found || previousProof.account <= 0 {
			r.reason = "replay_evidence_missing"
			return ctx
		}
		if len(items) < previousProof.count {
			r.reason = "incomplete_history"
			return ctx
		}
		r.previousAccount = previousProof.account
	}
	for i, item := range items {
		var ok bool
		r.digest, ok = appendBorrowReplayDigest(r.digest, item)
		if !ok {
			r.reason = "invalid_replay_input"
			return ctx
		}
		if previous != "" && i+1 == previousProof.count && r.digest != previousProof.digest {
			r.reason = "history_mismatch"
			return ctx
		}
	}
	r.count = len(items)
	return ctx
}

func borrowHTTPReplayInput(body []byte) ([]json.RawMessage, string) {
	if len(body) > borrowHTTPReplayMaxBytes {
		return nil, "replay_input_too_large"
	}
	if value := gjson.GetBytes(body, "conversation"); value.Exists() && value.Type != gjson.Null {
		return nil, "nonportable_context"
	}
	items, exists, err := openAIWSExtractNormalizedInputSequence(body)
	if err != nil || !exists || len(items) == 0 || len(items) > borrowHTTPReplayMaxItems {
		return nil, "invalid_replay_input"
	}
	if openAIWSRawItemsContainNonPortableContext(items) || borrowHTTPReplayHasAccountResources(gjson.ParseBytes(body)) {
		return nil, "nonportable_context"
	}
	if err := validateBorrowHTTPContinuationInput(body); err != nil {
		return nil, "incomplete_tool_context"
	}
	return items, ""
}

// Files, vector stores and explicit interpreter containers are owned by the
// upstream credential too. Their IDs cannot be made portable by replaying text.
func borrowHTTPReplayHasAccountResources(value gjson.Result) bool {
	blocked := false
	if !value.IsObject() && !value.IsArray() {
		return false
	}
	value.ForEach(func(key, child gjson.Result) bool {
		switch key.String() {
		case "file_id", "container_id":
			blocked = child.Type == gjson.String && child.String() != ""
		case "file_ids", "vector_store_ids":
			blocked = child.IsArray() && len(child.Array()) > 0
		case "container":
			blocked = child.Type == gjson.String && child.String() != "" && child.String() != "auto"
		}
		blocked = blocked || borrowHTTPReplayHasAccountResources(child)
		return !blocked
	})
	return blocked
}

func appendBorrowReplayDigest(previous [32]byte, item json.RawMessage) ([32]byte, bool) {
	var value any
	if decodeOpenAIJSONUseNumber(item, &value) != nil {
		return previous, false
	}
	// Normalize only protocol-equivalent message forms and nonsemantic output
	// metadata. Tool call IDs/arguments and every other field remain in the hash.
	if text, ok := value.(string); ok {
		value = map[string]any{"role": "user", "content": text}
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return previous, false
	}
	if role, ok := obj["role"].(string); ok && (obj["type"] == nil || obj["type"] == "message") {
		obj["type"] = "message"
		delete(obj, "id")
		delete(obj, "status")
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		if text, ok := obj["content"].(string); ok {
			obj["content"] = []any{map[string]any{"type": contentType, "text": text}}
		}
		if parts, ok := obj["content"].([]any); ok {
			for _, part := range parts {
				if text, ok := part.(map[string]any); ok {
					if annotations, ok := text["annotations"].([]any); ok && len(annotations) == 0 {
						delete(text, "annotations")
					}
				}
			}
		}
	} else if callID, ok := obj["call_id"].(string); ok && callID != "" {
		delete(obj, "id")
		delete(obj, "status")
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return previous, false
	}
	itemDigest := sha256.Sum256(encoded)
	var combined [64]byte
	copy(combined[:32], previous[:])
	copy(combined[32:], itemDigest[:])
	return sha256.Sum256(combined[:]), true
}

// Called with the final client-facing Responses payload (after tool restoration).
// Only an authoritative successful terminal with portable output grants proof.
func (s *OpenAIGatewayService) observeBorrowHTTPReplayResponse(ctx context.Context, account *Account, payload []byte, eventType string) {
	r := borrowHTTPReplayFromContext(ctx)
	if r == nil || r.reason != "" || account == nil || !account.IsOpenAI() || len(payload) > borrowHTTPReplayMaxBytes {
		return
	}
	root := gjson.ParseBytes(payload)
	if eventType != "" {
		if eventType != "response.completed" && eventType != "response.done" {
			return
		}
		root = root.Get("response")
	}
	if root.Get("status").String() != "completed" || root.Get("error").Type != gjson.Null && root.Get("error").Exists() {
		return
	}
	id := strings.TrimSpace(root.Get("id").String())
	output := root.Get("output")
	if id == "" || !output.IsArray() || len(output.Array()) == 0 {
		return
	}
	items := make([]json.RawMessage, 0, len(output.Array()))
	for _, item := range output.Array() {
		items = append(items, json.RawMessage(item.Raw))
	}
	if r.count+len(items) > borrowHTTPReplayMaxItems || openAIWSRawItemsContainNonPortableContext(items) || borrowHTTPReplayHasAccountResources(output) {
		return
	}
	digest := r.digest
	for _, item := range items {
		var ok bool
		digest, ok = appendBorrowReplayDigest(digest, item)
		if !ok {
			return
		}
	}
	key := r.key
	key.response = sha256.Sum256([]byte(id))
	s.borrowHTTPReplay.put(key, borrowHTTPReplayProof{digest: digest, count: r.count + len(items), account: account.ID, expires: time.Now().Add(min(borrowHTTPReplayTTL, s.openAIWSResponseStickyTTL()))})
}
