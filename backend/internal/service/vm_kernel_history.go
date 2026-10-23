package service

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	vmKernelContinuationBytes   = 64 << 20
	vmKernelContinuationEntries = 512
)

type vmKernelContinuationKey struct {
	groupID, userID, accountID int64
	responseID                 string
}

type vmKernelContinuationEntry struct {
	snapshot vmKernelWSSnapshot
	binding  vmkernel.SessionBinding
	expires  time.Time
}

// The shared cache holds only response context. Existing state-store ownership
// and routing checks remain authoritative on every read. A cache miss never
// authorizes importing another account's upstream history.
type vmKernelContinuationStore struct {
	mu      sync.Mutex
	entries map[vmKernelContinuationKey]vmKernelContinuationEntry
	order   []vmKernelContinuationKey
	bytes   int
}

var vmKernelContinuations vmKernelContinuationStore

// VM publications are serialized per response ID before writing the original
// owner/router store. Stripes stay bounded regardless of response cardinality.
var vmKernelContinuationPublicationLocks [64]sync.Mutex

func vmKernelContinuationPublicationLock(group int64, id string) *sync.Mutex {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d:%s", group, id)
	return &vmKernelContinuationPublicationLocks[h.Sum64()%uint64(len(vmKernelContinuationPublicationLocks))]
}

func (s *vmKernelContinuationStore) containsResponse(group int64, id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	for key := range s.entries {
		if key.groupID == group && key.responseID == id {
			return true
		}
	}
	return false
}

func (s *vmKernelContinuationStore) prune(now time.Time) {
	kept := s.order[:0]
	for _, key := range s.order {
		entry, ok := s.entries[key]
		if !ok {
			continue
		}
		if !now.Before(entry.expires) {
			s.bytes -= entry.snapshot.size
			delete(s.entries, key)
			continue
		}
		kept = append(kept, key)
	}
	s.order = kept
}

func (s *vmKernelContinuationStore) put(key vmKernelContinuationKey, entry vmKernelContinuationEntry, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	if entry.snapshot.size <= 0 || entry.snapshot.size > vmKernelWSHistoryLimit || !now.Before(entry.expires) {
		return fmt.Errorf("VM continuation snapshot is invalid")
	}
	if _, exists := s.entries[key]; exists {
		return fmt.Errorf("VM duplicate response id")
	}
	for len(s.order) > 0 && (len(s.order) >= vmKernelContinuationEntries || s.bytes+entry.snapshot.size > vmKernelContinuationBytes) {
		old := s.order[0]
		s.order = s.order[1:]
		s.bytes -= s.entries[old].snapshot.size
		delete(s.entries, old)
	}
	if s.entries == nil {
		s.entries = make(map[vmKernelContinuationKey]vmKernelContinuationEntry)
	}
	s.entries[key] = entry
	s.order = append(s.order, key)
	s.bytes += entry.snapshot.size
	return nil
}

func (s *vmKernelContinuationStore) get(key vmKernelContinuationKey, now time.Time) (vmKernelContinuationEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	entry, ok := s.entries[key]
	return entry, ok
}

func vmKernelContinuationOwner(c *gin.Context) (openAIHTTPResponseOwner, bool) {
	if c == nil {
		return openAIHTTPResponseOwner{}, false
	}
	raw, found := c.Get(openAIHTTPResponseOwnerContextKey)
	owner, ok := raw.(openAIHTTPResponseOwner)
	return owner, found && ok && owner.userID > 0 && owner.apiKeyID > 0
}

// prepareVMKernelContinuation is shared by HTTP and WS. It preserves an already
// bound connection's generation and attaches a snapshot binding for a fresh
// connection so Execute performs the final authoritative fence before dispatch.
func (s *OpenAIGatewayService) prepareVMKernelContinuation(ctx context.Context, c *gin.Context, account *Account, body []byte) (context.Context, []byte, error) {
	binding := vmkernel.BindingFromContext(ctx)
	if binding == nil {
		binding = &vmkernel.SessionBinding{}
		ctx = vmkernel.WithSessionBinding(ctx, binding)
	}
	previous := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	if previous == "" {
		return ctx, body, nil
	}
	owner, ok := vmKernelContinuationOwner(c)
	if !ok || account == nil {
		return ctx, nil, fmt.Errorf("VM continuation owner is unavailable")
	}
	group := getOpenAIGroupIDFromContext(c)
	allowed, err := s.ValidateOpenAIHTTPResponseOwner(ctx, group, previous, owner.userID, owner.apiKeyID)
	if err != nil || !allowed {
		return ctx, nil, fmt.Errorf("VM continuation does not belong to this owner")
	}
	accountID, err := s.getOpenAIWSStateStore().GetResponseAccount(ctx, group, previous)
	if err != nil || accountID != account.ID {
		return ctx, nil, fmt.Errorf("VM continuation account changed")
	}
	entry, ok := vmKernelContinuations.get(vmKernelContinuationKey{group, owner.userID, account.ID, previous}, time.Now())
	if !ok {
		return ctx, nil, fmt.Errorf("VM continuation context is unavailable; provide complete input")
	}
	if binding.AccountID != 0 && *binding != entry.binding {
		return ctx, nil, fmt.Errorf("VM continuation generation changed")
	}
	frame, err := sjson.SetBytes(body, "type", "response.create")
	if err != nil {
		return ctx, nil, err
	}
	history := &vmKernelWSHistory{snapshots: map[string]vmKernelWSSnapshot{previous: entry.snapshot}}
	replayed, err := history.prepare(frame)
	if err != nil {
		return ctx, nil, err
	}
	*binding = entry.binding
	return ctx, replayed, nil
}

// Call only after a fully completed native attempt, with the original replay
// body and the terminal response after restoring public tool names. Failure to
// remember history must not turn an already completed request into a retry.
func (s *OpenAIGatewayService) rememberVMKernelContinuation(ctx context.Context, c *gin.Context, account *Account, body, response []byte) error {
	owner, ok := vmKernelContinuationOwner(c)
	binding := vmkernel.BindingFromContext(ctx)
	if !ok || account == nil || binding == nil || binding.AccountID != account.ID || binding.Generation == 0 || binding.InstanceID == "" || binding.GuestBootID == "" {
		return nil // Internal probes and unowned callers never publish shared history.
	}
	if status := gjson.GetBytes(response, "status").String(); status != "" && status != "completed" {
		return fmt.Errorf("VM incomplete response cannot publish continuation history")
	}
	if failure := gjson.GetBytes(response, "error"); failure.Exists() && failure.Raw != "null" {
		return fmt.Errorf("VM failed response cannot publish continuation history")
	}
	frame, err := sjson.SetBytes(body, "type", "response.create")
	if err != nil {
		return err
	}
	history := &vmKernelWSHistory{}
	canonical, err := history.prepare(frame)
	if err != nil {
		return err
	}
	if err = history.complete(canonical, response); err != nil {
		return err
	}
	id := strings.TrimSpace(gjson.GetBytes(response, "id").String())
	entry := vmKernelContinuationEntry{snapshot: history.snapshots[id], binding: *binding}
	group := getOpenAIGroupIDFromContext(c)
	ttl := s.openAIWSResponseStickyTTL()
	if ttl <= 0 || ttl > time.Hour {
		ttl = time.Hour
	}
	bindCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIWSStateStoreRedisTimeout)
	defer cancel()
	publication := vmKernelContinuationPublicationLock(group, id)
	publication.Lock()
	defer publication.Unlock()
	store := s.getOpenAIWSStateStore()
	// Any pre-existing binding is immutable, including a duplicate belonging
	// to the same owner. Reject before touching either authoritative binding.
	if vmKernelContinuations.containsResponse(group, id, time.Now()) {
		return fmt.Errorf("VM duplicate response id")
	}
	priorAccount, err := store.GetResponseAccount(bindCtx, group, id)
	if err != nil {
		return err
	}
	_, _, hasOwner, err := store.GetHTTPResponseOwner(bindCtx, group, id)
	// Redis uses an explicit miss sentinel, unlike in-memory stores. Only an
	// actual absence permits publication; infrastructure failures stay closed.
	if err != nil && !errors.Is(err, ErrStickySessionNotFound) {
		return err
	}
	if priorAccount != 0 || hasOwner {
		return fmt.Errorf("VM duplicate response id")
	}
	if err = store.BindResponseAccount(bindCtx, group, id, account.ID, ttl); err != nil {
		return err
	}
	if err = store.BindHTTPResponseOwner(bindCtx, group, id, owner.userID, owner.apiKeyID, ttl); err != nil {
		return err
	}
	now := time.Now()
	entry.expires = now.Add(ttl)
	return vmKernelContinuations.put(vmKernelContinuationKey{group, owner.userID, account.ID, id}, entry, now)
}
