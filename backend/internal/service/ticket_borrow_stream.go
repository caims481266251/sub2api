package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	recoveryContext context.Context
	owner           *TicketBorrowService
	target, source  int64
	route           string
	pending         []byte
	dropping        bool
	bound           bool
	mu              sync.Mutex
}

func (b *borrowObservedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	rest := p[:n]
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
			} else {
				b.pending = append(b.pending, part...)
			}
		}
		if pos < 0 {
			break
		}
		if !b.dropping {
			b.line(b.pending)
		}
		b.pending = nil
		b.dropping = false
		rest = rest[pos+1:]
	}
	if err != nil {
		if err == io.EOF && len(b.pending) > 0 && !b.dropping {
			b.line(b.pending)
		}
	}
	return n, err
}
func (b *borrowObservedBody) line(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	var event struct {
		Type     string `json:"type"`
		Response struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
		return
	}
	payload := bytes.TrimSpace(line[5:])
	BorrowBusinessRecoveryFromContext(b.recoveryContext).ObserveSSE(b.recoveryContext, payload, event.Type)
	if !b.bound && event.Response.ID != "" && strings.HasPrefix(event.Type, "response.") {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := b.owner.repo.BindSession(ctx, b.target, borrowResponseKey(event.Response.ID), b.route)
		cancel()
		b.bound = err == nil
	}
}
