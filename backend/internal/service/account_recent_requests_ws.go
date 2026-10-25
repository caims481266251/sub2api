package service

import (
	"context"
	"sync"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// Frame transport already serializes response.create turns. Keep metadata only;
// the existing envelope and terminal helpers remain the protocol authority.
type recentRequestsFrameConn struct {
	scope    string
	ttftMode string
	openaiwsv2.FrameConn
	cache   AccountRecentRequestsCache
	account *Account
	mu      sync.Mutex
	active  *accountRequestAttempt
}

func (c *recentRequestsFrameConn) current() *accountRequestAttempt {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}
func (c *recentRequestsFrameConn) WriteFrame(ctx context.Context, typ coderws.MessageType, payload []byte) error {
	var attempt *accountRequestAttempt
	if gjson.GetBytes(payload, "type").String() == "response.create" {
		if c.scope != "" {
			ctx = context.WithValue(ctx, recentRequestScopeContextKey{}, recentRequestScope{c.account.ID, c.scope})
		}
		attempt = beginAccountRequestAttempt(ctx, c.cache, c.account, gjson.GetBytes(payload, "model").String())
		c.mu.Lock()
		previous := c.active
		c.active = attempt
		c.mu.Unlock()
		previous.finish(nil)
	}
	err := c.FrameConn.WriteFrame(ctx, typ, payload)
	if err != nil {
		attempt.finish(err)
	}
	return err
}
func (c *recentRequestsFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	typ, payload, err := c.FrameConn.ReadFrame(ctx)
	a := c.current()
	if a != nil && len(payload) > 0 {
		eventType, _, _ := parseOpenAIWSEventEnvelope(payload)
		observeRecentWSEvent(a, payload, eventType, c.ttftMode)
		if isOpenAIWSTerminalEvent(eventType) {
			a.finish(nil)
		}
	}
	if err != nil {
		a.finish(err)
	}
	return typ, payload, err
}
func (c *recentRequestsFrameConn) Close() error {
	err := c.FrameConn.Close()
	c.current().finish(nil)
	return err
}

func observeRecentWSEvent(a *accountRequestAttempt, payload []byte, eventType, mode string) {
	terminal := ""
	if isOpenAIWSTerminalEvent(eventType) {
		terminal = "failed"
		if recentResponseTerminalSuccessful(payload, eventType) {
			terminal = "completed"
		}
	}
	a.observe(recentResponseStartsTTFT(string(payload), eventType, mode), terminal)
}
