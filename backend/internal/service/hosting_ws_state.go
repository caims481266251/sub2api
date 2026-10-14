package service

import (
	"context"
	"fmt"
	"time"
)

// Response IDs are supplied by the client and may repeat across users. Scope
// both Redis account bindings and local upstream-connection bindings.
type hostedWSStateStore struct {
	OpenAIWSStateStore
	prefix string
}

func (s *OpenAIGatewayService) getOpenAIWSStateStoreForContext(ctx context.Context) OpenAIWSStateStore {
	store := s.getOpenAIWSStateStore()
	key := hostingKeyFromContext(ctx)
	if store == nil || key == nil {
		return store
	}
	poolID := int64(0)
	if key.HostingPoolID != nil {
		poolID = *key.HostingPoolID
	}
	return &hostedWSStateStore{store, fmt.Sprintf("hosting:%d:%d:%d:%t:", key.UserID, key.ID, poolID, key.HostingFallback)}
}
func (s *hostedWSStateStore) BindResponseAccount(ctx context.Context, g int64, r string, a int64, t time.Duration) error {
	return s.OpenAIWSStateStore.BindResponseAccount(ctx, g, s.prefix+r, a, t)
}
func (s *hostedWSStateStore) GetResponseAccount(ctx context.Context, g int64, r string) (int64, error) {
	return s.OpenAIWSStateStore.GetResponseAccount(ctx, g, s.prefix+r)
}
func (s *hostedWSStateStore) DeleteResponseAccount(ctx context.Context, g int64, r string) error {
	return s.OpenAIWSStateStore.DeleteResponseAccount(ctx, g, s.prefix+r)
}
func (s *hostedWSStateStore) BindHTTPResponseOwner(ctx context.Context, g int64, r string, u, k int64, t time.Duration) error {
	return s.OpenAIWSStateStore.BindHTTPResponseOwner(ctx, g, s.prefix+r, u, k, t)
}
func (s *hostedWSStateStore) GetHTTPResponseOwner(ctx context.Context, g int64, r string) (int64, int64, bool, error) {
	return s.OpenAIWSStateStore.GetHTTPResponseOwner(ctx, g, s.prefix+r)
}
func (s *hostedWSStateStore) BindResponseConn(r, c string, t time.Duration) {
	s.OpenAIWSStateStore.BindResponseConn(s.prefix+r, c, t)
}
func (s *hostedWSStateStore) GetResponseConn(r string) (string, bool) {
	return s.OpenAIWSStateStore.GetResponseConn(s.prefix + r)
}
func (s *hostedWSStateStore) DeleteResponseConn(r string) {
	s.OpenAIWSStateStore.DeleteResponseConn(s.prefix + r)
}
