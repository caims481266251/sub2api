package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

func borrowSessionKey(h http.Header) string {
	// Match the gateway's existing conversation separation; retain only a hash.
	id := h.Get("session_id")
	if id == "" {
		id = h.Get("session-id")
	}
	thread := h.Get("x-codex-thread-id")
	conversation := h.Get("conversation_id")
	if id == "" && thread == "" && conversation == "" {
		return ""
	}
	return borrowHash("session\x00" + id + "\x00" + thread + "\x00" + conversation)
}
func borrowResponseKey(id string) string {
	if id == "" {
		return ""
	}
	return borrowHash("response\x00" + id)
}

// Observe bounded SSE metadata only to bind continuation IDs. Business outcomes
// are owned by the normal request logger, never by borrowing telemetry.
type borrowObservedBody struct {
	io.ReadCloser
	outcomeBytes     int
	outcomeFrames    openAISSEDataAccumulator
	outcome          *borrowRouteResponseObserver
	owner            *TicketBorrowService
	target           int64
	effectiveSession string
	httpContinuation bool
	route            string
	pending          []byte
	dropping         bool
	bound            bool
	bindingDone      <-chan struct{}
	deferred         []byte
	deferredErr      error
	mu               sync.Mutex
}

func (b *borrowObservedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	if len(b.deferred) > 0 {
		<-b.bindingDone
		n := copy(p, b.deferred)
		b.deferred = b.deferred[n:]
		var err error
		if len(b.deferred) == 0 {
			b.deferred = nil
			err, b.deferredErr = b.deferredErr, nil
		}
		b.mu.Unlock()
		return n, err
	}
	b.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	rest := p[:n]
	terminalAt := -1
	lineStart := 0
	for len(rest) > 0 {
		pos := bytes.IndexByte(rest, '\n')
		part := rest
		if pos >= 0 {
			part = rest[:pos]
		}
		if !b.dropping {
			if len(b.pending)+len(part) > 65536 {
				b.pending = nil
				b.dropping = true
				if b.outcome != nil {
					b.outcome.finish(false, "", nil)
				}
			} else {
				b.pending = append(b.pending, part...)
			}
		}
		if pos < 0 {
			break
		}
		if !b.dropping {
			if b.line(b.pending) && terminalAt < 0 {
				terminalAt = lineStart
			}
		}
		b.pending = nil
		b.dropping = false
		rest = rest[pos+1:]
		lineStart = n - len(rest)
	}
	if err != nil {
		if err == io.EOF && len(b.pending) > 0 && !b.dropping {
			if b.line(b.pending) && terminalAt < 0 {
				terminalAt = lineStart
			}
		}
	}
	if terminalAt >= 0 && b.bindingDone != nil {
		select {
		case <-b.bindingDone:
		default:
			// Flush any earlier deltas in this same network chunk first. Only
			// completion waits for the bounded metadata write, so a continuation
			// arriving at another instance after completion can find its route.
			if terminalAt > 0 {
				b.deferred = append([]byte(nil), p[terminalAt:n]...)
				b.deferredErr = err
				return terminalAt, nil
			}
			<-b.bindingDone
		}
	}
	if err == io.EOF && b.outcome != nil {
		b.outcomeFrames.Flush(b.outcome.event)
	}
	if err != nil && b.outcome != nil && b.outcome.ctx.Err() == nil {
		b.outcome.finish(false, "route_incomplete_response", nil)
	}
	return n, err
}
func borrowTerminalEvent(typ string) bool {
	switch typ {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		return true
	default:
		return false
	}
}

func (b *borrowObservedBody) line(line []byte) bool {
	if b.outcome != nil {
		b.outcomeBytes += len(line)
		if b.outcomeBytes > 1<<20 {
			// Observation limits cannot turn a valid large business output
			// into a route failure. The caller still receives the whole stream.
			b.outcome.finish(false, "", nil)
			b.outcomeFrames = openAISSEDataAccumulator{}
		} else {
			b.outcomeFrames.AddLine(string(line), b.outcome.event)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			b.outcomeBytes = 0
		}
	}
	if bytes.HasPrefix(line, []byte("event:")) {
		return borrowTerminalEvent(string(bytes.TrimSpace(line[6:])))
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(line[5:]), []byte("[DONE]")) {
		return true
	}
	var event struct {
		Type     string `json:"type"`
		Response struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
		return false
	}
	if !b.bound && event.Response.ID != "" && strings.HasPrefix(event.Type, "response.") {
		b.bound = true
		b.bindingDone = b.owner.bindResponseAsync(b.target, borrowResponseKey(event.Response.ID), b.route, b.effectiveSession, b.httpContinuation)
	}
	return borrowTerminalEvent(event.Type)
}

// Closing also joins the bounded metadata write on partial/aborted responses.
func (b *borrowObservedBody) Close() error {
	err := b.ReadCloser.Close()
	b.mu.Lock()
	done := b.bindingDone
	b.mu.Unlock()
	if done != nil {
		<-done
	}
	return err
}

type borrowResponseBindingKey struct {
	target   int64
	response string
}

func (s *TicketBorrowService) bindResponseAsync(target int64, response, route, session string, continuation bool) <-chan struct{} {
	key := borrowResponseBindingKey{target, response}
	done := make(chan struct{})
	existing, loaded := s.responseBindings.LoadOrStore(key, done)
	if loaded {
		return existing.(chan struct{})
	}
	go func() {
		defer s.responseBindings.Delete(key)
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var err error
		if continuation {
			err = s.repo.(BorrowHTTPContinuationRepository).BindHTTPContinuationResponse(ctx, target, response, route, session)
		} else {
			_, err = s.repo.BindSession(ctx, target, response, route)
		}
		if err != nil {
			slog.Warn("borrow_response_binding_failed", "target_id", target, "route_id", route)
		}
	}()
	return done
}

func (s *TicketBorrowService) waitResponseBinding(ctx context.Context, target int64, response string) error {
	if value, ok := s.responseBindings.Load(borrowResponseBindingKey{target, response}); ok {
		select {
		case <-value.(chan struct{}):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
