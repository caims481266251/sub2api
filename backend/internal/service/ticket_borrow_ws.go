package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// A socket's routing cookie cannot change after its handshake. Borrowing only
// adds route affinity and admission; payloads and the ordinary WS policy remain intact.
type borrowWSUse struct {
	service  *TicketBorrowService
	route    *BorrowRoute
	use      *borrowRouteUse
	mu       sync.Mutex
	bindings map[string]<-chan struct{}
}

func (s *TicketBorrowService) reserveWS(ctx, lifetime context.Context, account *Account, endpoint, model, previous string, headers http.Header) (*borrowWSUse, error) {
	if s == nil || account == nil || borrowInternal(ctx) {
		return nil, nil
	}
	target, err := s.repo.Target(ctx, account.ID)
	if err != nil {
		return nil, ErrBorrowUnavailable
	}
	if !target.Enabled {
		return nil, nil
	}
	if strings.TrimSpace(model) == "" {
		return nil, ErrBorrowUnavailable
	}
	latest, err := s.owner.accountRepo.GetByID(ctx, account.ID)
	if err != nil || latest == nil || !latest.IsSchedulable() {
		return nil, ErrBorrowUnavailable
	}
	_, identity, err := s.targetEnvironment(ctx, latest)
	if err != nil {
		return nil, ErrBorrowUnavailable
	}
	hint, hasHint := ctx.Value(borrowBindingKey{}).(borrowBindingHint)
	if !hasHint {
		hint.Session = borrowSessionKey(headers)
	}
	if previous != "" {
		response := borrowResponseKey(previous)
		if hint.Response != "" && hint.Response != response {
			return nil, ErrBorrowUnavailable
		}
		hint.Response = response
	}
	preferred := ""
	if hint.Response != "" {
		preferred, err = s.borrowPreferredRoute(ctx, account.ID, hint)
		if err != nil {
			return nil, err
		}
	}
	allocator, ok := s.repo.(BorrowRouteLeaseRepository)
	if !ok {
		return nil, ErrBorrowUnavailable
	}
	lease := uuid.NewString()
	// A new full-input turn can use the latest route. Native continuations stay
	// on the response's exact route; an old client-session hash must not strand
	// otherwise independent turns on an expired ticket.
	route, err := allocator.ReserveRoute(ctx, account.ID, identity, preferred, "", lease)
	if err != nil {
		return nil, ErrBorrowUnavailable
	}
	use := newBorrowRouteUse(lifetime, allocator, lease, 15*time.Second)
	u, err := url.Parse(endpoint)
	if err != nil {
		use.finish()
		return nil, ErrBorrowUnavailable
	}
	if u.Scheme == "wss" {
		u.Scheme = "https"
	} else if u.Scheme == "ws" {
		u.Scheme = "http"
	}
	if route.Secret.Endpoint != u.Scheme+"://"+u.Host+u.EscapedPath() || u.RawQuery != "" {
		use.finish()
		return nil, ErrBorrowUnavailable
	}
	return &borrowWSUse{service: s, route: route, use: use, bindings: make(map[string]<-chan struct{})}, nil
}

func (s *TicketBorrowService) checkWSRoute(ctx context.Context, accountID int64, route *BorrowRoute) error {
	target, err := s.repo.Target(ctx, accountID)
	if err != nil {
		return ErrBorrowUnavailable
	}
	if !target.Enabled {
		if route != nil {
			return ErrBorrowUnavailable
		}
		return nil
	}
	if route == nil {
		return ErrBorrowUnavailable
	}
	account, err := s.owner.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil || !account.IsSchedulable() {
		return ErrBorrowUnavailable
	}
	_, identity, err := s.targetEnvironment(ctx, account)
	if err != nil || identity != route.IdentityHash {
		return ErrBorrowUnavailable
	}
	_, err = s.repo.Route(ctx, accountID, identity, route.ID)
	if err != nil {
		return ErrBorrowUnavailable
	}
	return nil
}

func (b *borrowWSUse) check(ctx context.Context, value any) error {
	if b == nil {
		return nil
	}
	var raw []byte
	switch v := value.(type) {
	case json.RawMessage:
		raw = v
	case openAIWSPreparedJSON:
		raw = v
	case []byte:
		raw = v
	default:
		var err error
		raw, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	typ := gjson.GetBytes(raw, "type").String()
	// Control frames must remain usable while an admitted turn drains.
	if typ == "response.cancel" || typ == "ping" {
		return nil
	}
	if err := b.use.ctx.Err(); err != nil {
		return err
	}
	if err := b.service.checkWSRoute(ctx, b.route.TargetID, b.route); err != nil {
		return err
	}
	if previous := gjson.GetBytes(raw, "previous_response_id").String(); previous != "" {
		key := borrowResponseKey(previous)
		if err := b.service.waitResponseBinding(ctx, b.route.TargetID, key); err != nil {
			return err
		}
		route, found, err := b.service.repo.LookupSession(ctx, b.route.TargetID, key)
		if err != nil || !found || route != b.route.ID {
			return ErrBorrowUnavailable
		}
	}
	if err := b.use.repo.RenewRoute(ctx, b.use.id); err != nil {
		return ErrBorrowUnavailable
	}
	return nil
}

func (b *borrowWSUse) observe(raw []byte) {
	if b == nil {
		return
	}
	id := gjson.GetBytes(raw, "response.id").String()
	if id == "" {
		return
	}
	b.mu.Lock()
	done, ok := b.bindings[id]
	if !ok {
		done = b.service.bindResponseAsync(b.route.TargetID, borrowResponseKey(id), b.route.ID, "", false)
		b.bindings[id] = done
	}
	b.mu.Unlock()
	if borrowTerminalEvent(gjson.GetBytes(raw, "type").String()) {
		<-done
		b.mu.Lock()
		delete(b.bindings, id)
		b.mu.Unlock()
	}
}

func (b *borrowWSUse) finish() {
	if b == nil {
		return
	}
	b.use.finish()
	b.mu.Lock()
	waits := make([]<-chan struct{}, 0, len(b.bindings))
	for _, done := range b.bindings {
		waits = append(waits, done)
	}
	b.mu.Unlock()
	for _, done := range waits {
		<-done
	}
}

func injectBorrowWSCookie(headers http.Header, route *BorrowRoute) {
	if route != nil {
		replaceBorrowCookie(&http.Request{Header: headers}, &route.Secret.Cookie)
	}
}

func (s *TicketBorrowService) observeWSHandshake(ctx context.Context, route *BorrowRoute, headers http.Header, status int) error {
	if route == nil {
		return nil
	}
	s.observeBorrowBusinessResponse((&http.Request{Header: http.Header{}}).WithContext(ctx), route, &http.Response{StatusCode: status, Header: headers})
	if status == http.StatusUnauthorized {
		return ErrBorrowUnavailable
	}
	return nil
}

type borrowWSFrameConn struct {
	openaiwsv2.FrameConn
	borrow    *borrowWSUse
	service   *TicketBorrowService
	accountID int64
}

func (c *borrowWSFrameConn) WriteFrame(ctx context.Context, typ coderws.MessageType, raw []byte) error {
	if typ == coderws.MessageText {
		if err := c.borrow.check(ctx, raw); err != nil {
			return err
		}
		if c.borrow == nil && c.service != nil {
			event := gjson.GetBytes(raw, "type").String()
			if event != "response.cancel" && event != "ping" {
				if err := c.service.checkWSRoute(ctx, c.accountID, nil); err != nil {
					return err
				}
			}
		}
	}
	return c.FrameConn.WriteFrame(ctx, typ, raw)
}
func (c *borrowWSFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	typ, raw, err := c.FrameConn.ReadFrame(ctx)
	if typ == coderws.MessageText {
		c.borrow.observe(raw)
	}
	return typ, raw, err
}

type borrowWSCloseFunc func()

func (f borrowWSCloseFunc) Close() error { f(); return nil }

var _ io.Closer = borrowWSCloseFunc(nil)
