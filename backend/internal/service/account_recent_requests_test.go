package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type recentRequestsTestCache struct {
	mu      sync.Mutex
	records []AccountRecentRequest
	ctxErr  error
	scope   string
}

func (c *recentRequestsTestCache) Append(ctx context.Context, scope string, r AccountRecentRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	c.ctxErr, c.scope = ctx.Err(), scope
	return nil
}
func (*recentRequestsTestCache) Batch(context.Context, map[int64]string) (map[int64][]AccountRecentRequest, error) {
	return nil, nil
}
func (*recentRequestsTestCache) Close() error { return nil }

func recentRequestsTestAccount() *Account {
	return &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-secret"}}
}

func TestAccountRecentRequestCompletionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, payload, outcome string
		err                    error
	}{
		{"http200_without_terminal", "", "unknown", nil},
		{"completed", `{"type":"response.completed","response":{"status":"completed"}}`, "success", nil},
		{"done", `{"type":"response.done","response":{"status":"completed"}}`, "success", nil},
		{"completed_error", `{"type":"response.completed","response":{"status":"completed","error":{"message":"private"}}}`, "failure", nil},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete"}}`, "failure", nil},
		{"parser_error_after_completed", `{"type":"response.completed","response":{"status":"completed"}}`, "failure", errors.New("invalid response includes secret")},
		{"transport_error", "", "failure", errors.New("dial private-url")},
		{"cancelled", "", "cancelled", context.Canceled},
		{"completed_cleanup_cancel", `{"type":"response.completed","response":{"status":"completed"}}`, "success", context.Canceled},
		{"timeout", "", "failure", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := &recentRequestsTestCache{}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			req, attempt := beginRecentHTTPRequest(req, cache, recentRequestsTestAccount())
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
			attempt.response(req, resp, nil)
			ctx := recentRequestResponseContext(context.Background(), resp)
			if tc.payload != "" {
				kind, _, _ := parseOpenAIWSEventEnvelope([]byte(tc.payload))
				observeRecentProtocolEvent(ctx, []byte(tc.payload), kind, OpenAITTFTModeSemantic)
			}
			recentRequestFailed(ctx, tc.err)
			require.NoError(t, resp.Body.Close())
			attempt.finish(nil)
			require.Len(t, cache.records, 1)
			require.Equal(t, tc.outcome, cache.records[0].Outcome)
			require.Nil(t, cache.records[0].FirstTokenMs)
			require.NotContains(t, cache.records[0].ErrorMessage, "private")
			require.NotContains(t, cache.records[0].ErrorMessage, "secret")
		})
	}
}

func TestAccountRecentRequestTerminalFirstOutputRequiresContent(t *testing.T) {
	for _, mode := range []string{OpenAITTFTModeSemantic, OpenAITTFTModeVisible} {
		for _, tc := range []struct {
			name, payload, event string
			first                bool
		}{
			{"empty_completed", `{"type":"response.completed","response":{"status":"completed"}}`, "response.completed", false},
			{"empty_done", `{"type":"response.done","response":{"status":"completed","output":[]}}`, "response.done", false},
			{"failed", `{"type":"response.failed","response":{"status":"failed","error":{"message":"failed"}}}`, "response.failed", false},
			{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete"}}`, "response.incomplete", false},
			{"bare_error", `{"type":"error","error":{"message":"invalid request"}}`, "error", false},
			{"terminal_text", `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}}`, "response.completed", true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				require.Equal(t, tc.first, recentResponseStartsTTFT(tc.payload, tc.event, mode))
			})
		}
	}
}

func TestAccountRecentRequestCancellationUsesIndependentWriteContext(t *testing.T) {
	cache := &recentRequestsTestCache{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), qualityProbeContextKey{}, true))
	a := beginAccountRequestAttempt(ctx, cache, recentRequestsTestAccount(), "gpt-test")
	cancel()
	a.finish(ctx.Err())
	require.Len(t, cache.records, 1)
	require.NoError(t, cache.ctxErr)
	require.Equal(t, "quality", cache.records[0].Source)
	require.Equal(t, "cancelled", cache.records[0].Outcome)
	require.Equal(t, AccountActivityScope(recentRequestsTestAccount()), cache.scope)
}

func TestAccountRecentRequestModelMetadataDoesNotReadReplayBody(t *testing.T) {
	cache := &recentRequestsTestCache{}
	body := []byte(`{"input":"` + strings.Repeat("large history", 1000) + `","model":"gpt-6.1-sol"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	req = req.WithContext(withRecentRequestModel(req.Context(), body))
	// GetBody can itself be instrumented as a transport-replay signal. Metadata
	// known at request construction must not touch it, even when model is late.
	req.GetBody = func() (io.ReadCloser, error) { t.Fatal("unexpected replay-body read"); return nil, nil }
	_, a := beginRecentHTTPRequest(req, cache, recentRequestsTestAccount())
	a.finish(nil)
	require.Equal(t, "gpt-6.1-sol", cache.records[0].Model)
}

func TestAccountRecentRequestQualityWrappedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &recentRequestsTestCache{}
	ctx := context.WithValue(context.Background(), qualityProbeContextKey{}, true)
	req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	req, a := beginRecentHTTPRequest(req, cache, recentRequestsTestAccount())
	wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n" + "data: {\"type\":\"response.done\",\"response\":{\"status\":\"completed\"}}\n\n"
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire))}
	a.response(req, resp, nil)
	// Match ticket/phase wrappers: the immediate Body is no longer our type.
	wrapped := struct{ io.ReadCloser }{resp.Body}
	resp.Body = wrapped
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req.WithContext(recentRequestResponseContext(ctx, resp))
	svc := &AccountTestService{}
	require.NoError(t, svc.processOpenAIStream(c, resp.Body))
	require.NoError(t, resp.Body.Close())
	require.Len(t, cache.records, 1)
	require.Equal(t, "success", cache.records[0].Outcome)
	require.Equal(t, "quality", cache.records[0].Source)
	require.NotNil(t, cache.records[0].FirstTokenMs)
}

type recentRequestsTestFrames struct{ payloads [][]byte }

func (*recentRequestsTestFrames) WriteFrame(context.Context, coderws.MessageType, []byte) error {
	return nil
}
func (f *recentRequestsTestFrames) ReadFrame(context.Context) (coderws.MessageType, []byte, error) {
	if len(f.payloads) == 0 {
		return coderws.MessageText, nil, io.EOF
	}
	p := f.payloads[0]
	f.payloads = f.payloads[1:]
	return coderws.MessageText, p, nil
}
func (*recentRequestsTestFrames) Close() error { return nil }

func TestAccountRecentRequestWSTurnsHaveSeparateTimers(t *testing.T) {
	cache := &recentRequestsTestCache{}
	frames := &recentRequestsTestFrames{payloads: [][]byte{
		[]byte(`{"type":"response.created"}`),
		[]byte(`{"type":"response.output_text.delta","delta":"hello"}`),
		[]byte(`{"type":"response.done","response":{"status":"completed"}}`),
		[]byte(`{"type":"response.failed","response":{"status":"failed"}}`),
	}}
	conn := &recentRequestsFrameConn{FrameConn: frames, cache: cache, account: recentRequestsTestAccount()}
	ctx := context.Background()
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"session.update"}`)))
	require.Nil(t, conn.current(), "handshake/session frames are not generation attempts")
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"first"}`)))
	conn.current().started = time.Now().Add(-20 * time.Second)
	for i := 0; i < 3; i++ {
		_, _, err := conn.ReadFrame(ctx)
		require.NoError(t, err)
	}
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"second"}`)))
	_, _, err := conn.ReadFrame(ctx)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.Len(t, cache.records, 2)
	require.Equal(t, "success", cache.records[0].Outcome)
	require.GreaterOrEqual(t, *cache.records[0].FirstTokenMs, int64(20_000))
	require.Equal(t, "failure", cache.records[1].Outcome)
	require.Nil(t, cache.records[1].FirstTokenMs)
	require.Less(t, cache.records[1].DurationMs, int64(10_000))
	require.NotEqual(t, cache.records[0].AttemptID, cache.records[1].AttemptID)
}
