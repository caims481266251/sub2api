package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const uploadKeepaliveRequest = `{"model":"gpt-5.5","stream":true,"input":"synthetic","instructions":"synthetic"}`
const uploadKeepaliveResponse = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_upload\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}}\n\n"

type uploadWaitTestUpstream struct {
	HTTPUpstream
	do func(*http.Request) (*http.Response, error)
}

func (u *uploadWaitTestUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.do(req)
}

type uploadKeepaliveSignalWriter struct {
	gin.ResponseWriter
	once    sync.Once
	flushed chan struct{}
}

func (w *uploadKeepaliveSignalWriter) Flush() {
	w.ResponseWriter.Flush()
	w.once.Do(func() { close(w.flushed) })
}

func uploadKeepaliveAccount(passthrough bool) *Account {
	return &Account{
		ID: 742, Name: "ordinary fallback", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "synthetic"},
		Extra:       map[string]any{"openai_passthrough": passthrough},
	}
}

func TestOpenAIUploadKeepaliveBeforeResponseHeaders(t *testing.T) {
	for _, passthrough := range []bool{true, false} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			c, rec := newPassthroughKeepaliveTestContext(t)
			originalWriter := c.Writer
			signal := &uploadKeepaliveSignalWriter{ResponseWriter: originalWriter, flushed: make(chan struct{})}
			c.Writer = signal
			// The upstream refuses to read the request or send headers until the
			// downstream receives a heartbeat. No real provider is contacted.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-signal.flushed:
				case <-time.After(4 * time.Second):
					t.Error("downstream stayed silent during upstream header wait")
					w.WriteHeader(http.StatusGatewayTimeout)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, uploadKeepaliveResponse)
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			svc := &OpenAIGatewayService{
				cfg:          &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1}},
				httpUpstream: &sseLifecycleLoopbackUpstream{client: server.Client(), target: target},
			}
			result, err := svc.Forward(c.Request.Context(), c, uploadKeepaliveAccount(passthrough), []byte(uploadKeepaliveRequest))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 5, result.Usage.OutputTokens)
			require.False(t, result.ClientDisconnect)
			require.Same(t, signal, c.Writer, "the heartbeat must release the request writer")
			require.True(t, strings.HasPrefix(rec.Body.String(), ": keepalive\n\n"))
			require.Contains(t, rec.Body.String(), "response.completed")
			require.Equal(t, "text/event-stream", rec.Result().Header.Get("Content-Type"))
			require.True(t, openAIStreamClientOutputStarted(c, false))
		})
	}
}

func TestOpenAIUploadKeepaliveErrorProtocol(t *testing.T) {
	for _, passthrough := range []bool{true, false} {
		for _, slow := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%t/slow=%t", passthrough, slow), func(t *testing.T) {
				c, rec := newPassthroughKeepaliveTestContext(t)
				signal := &uploadKeepaliveSignalWriter{ResponseWriter: c.Writer, flushed: make(chan struct{})}
				c.Writer = signal
				calls := 0
				upstream := &uploadWaitTestUpstream{do: func(_ *http.Request) (*http.Response, error) {
					calls++
					if slow {
						select {
						case <-signal.flushed:
						case <-time.After(4 * time.Second):
							return nil, errors.New("missing upload heartbeat")
						}
					}
					return &http.Response{StatusCode: http.StatusBadRequest,
						Header: http.Header{"Content-Type": {"application/json"}},
						Body:   io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","code":"bad_field","param":"input","message":"input is invalid"}}`)),
					}, nil
				}}
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1}}, httpUpstream: upstream}
				_, err := svc.Forward(c.Request.Context(), c, uploadKeepaliveAccount(passthrough), []byte(uploadKeepaliveRequest))
				require.Error(t, err)
				require.Equal(t, 1, calls, "a deterministic error must not replay")
				if !slow {
					require.Equal(t, http.StatusBadRequest, rec.Code)
					require.True(t, strings.HasPrefix(rec.Body.String(), "{"))
					require.NotContains(t, rec.Body.String(), "keepalive")
					return
				}
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, "text/event-stream", rec.Result().Header.Get("Content-Type"))
				events := parseCompactBridgeSSE(t, stripKeepaliveComments(rec.Body.String()))
				require.Len(t, events, 1)
				require.Equal(t, "response.failed", events[0][0])
				streamErr, ok := GetOpsStreamError(c)
				require.True(t, ok)
				require.Equal(t, http.StatusBadRequest, streamErr.IntendedStatus)
				require.True(t, IsResponseCommitted(c))
				if !passthrough {
					require.Contains(t, events[0][1], `"code":"bad_field"`)
					require.Contains(t, events[0][1], `"param":"input"`)
				}
			})
		}
	}
}

func TestOpenAIUploadKeepaliveRearmPreservesFailover(t *testing.T) {
	c, rec := newPassthroughKeepaliveTestContext(t)
	original := c.Writer
	stopFirst := startOpenAISSEKeepalive(c, time.Hour)
	first := c.MustGet(openAICompactSSEKeepaliveKey).(*openAICompactSSEKeepalive)
	require.True(t, first.beat())
	stopSecond := startOpenAISSEKeepalive(c, time.Hour)
	defer stopSecond()
	second := c.MustGet(openAICompactSSEKeepaliveKey).(*openAICompactSSEKeepalive)
	stopFirst() // An outer compact wait may defer its stop until after rearming.
	require.True(t, second.beat())
	require.False(t, first.beat())
	require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
	require.False(t, openAIStreamClientOutputStarted(c, false))
	require.True(t, StopOpenAICompactSSEKeepaliveCommitted(c))
	stopSecond()
	require.Same(t, original, c.Writer)
	require.Equal(t, 2, strings.Count(rec.Body.String(), ": keepalive"))
	_, err := c.Writer.WriteString("data: real\n\n")
	require.NoError(t, err)
	require.Equal(t, len("data: real\n\n"), OpenAICompactKeepaliveAdjustedWrittenSize(c))
}

func TestOpenAIUploadKeepaliveFailedWriteSurvivesRearm(t *testing.T) {
	c, _ := newPassthroughKeepaliveTestContext(t)
	c.Writer = &failingGinWriter{ResponseWriter: c.Writer, failAfter: 0}
	stop := startOpenAISSEKeepalive(c, time.Hour)
	first := c.MustGet(openAICompactSSEKeepaliveKey).(*openAICompactSSEKeepalive)
	require.False(t, first.beat())
	stop()
	stop = startOpenAISSEKeepalive(c, time.Hour)
	defer stop()
	require.False(t, c.MustGet(openAICompactSSEKeepaliveKey).(*openAICompactSSEKeepalive).beat())
	require.True(t, OpenAIDownstreamDisconnected(c))
}

func TestOpenAIUploadKeepaliveDisconnectDrainsAndClassifies(t *testing.T) {
	for _, passthrough := range []bool{true, false} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%t/canceled=%t", passthrough, canceled), func(t *testing.T) {
				c, _ := newPassthroughKeepaliveTestContext(t)
				ctx, cancel := context.WithCancel(c.Request.Context())
				defer cancel()
				collector := requesttiming.New(time.Now(), -1)
				ctx = requesttiming.With(ctx, collector)
				c.Request = c.Request.WithContext(ctx)
				if !canceled {
					c.Writer = &failingGinWriter{ResponseWriter: c.Writer, failAfter: 0}
				}
				calls := 0
				upstream := &uploadWaitTestUpstream{do: func(req *http.Request) (*http.Response, error) {
					calls++
					if canceled {
						cancel()
					}
					require.NoError(t, req.Context().Err(), "ordinary upstream lifetime remains detached for billing")
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(uploadKeepaliveResponse))}, nil
				}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				result, err := svc.Forward(ctx, c, uploadKeepaliveAccount(passthrough), []byte(uploadKeepaliveRequest))
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, result.ClientDisconnect)
				require.False(t, OpsStreamCompletedForAccount(c, 742))
				require.Equal(t, 3, result.Usage.InputTokens)
				require.Equal(t, 5, result.Usage.OutputTokens)
				require.Equal(t, 1, calls)
				collector.Finish(200, canceled)
				collector.WhenFinished(func(snapshot requesttiming.Snapshot) {
					require.Equal(t, "client_disconnected", snapshot.Outcome)
					require.True(t, snapshot.ClientDisconnect)
					require.Equal(t, "completed", snapshot.Terminal)
				})
			})
		}
	}
}

func TestOpenAIUploadKeepaliveDoesNotChangeNonStreamingOrDisabledRequests(t *testing.T) {
	for _, tc := range []struct {
		stream   bool
		interval int
	}{{false, 10}, {true, 0}} {
		c, rec := newPassthroughKeepaliveTestContext(t)
		original := c.Writer
		svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: tc.interval}}}
		stop := svc.startOpenAIUpstreamWaitKeepalive(c, tc.stream)
		stop()
		require.Same(t, original, c.Writer)
		require.Zero(t, rec.Body.Len())
		require.False(t, StopOpenAICompactSSEKeepaliveCommitted(c))
	}
}

func TestOpenAIUploadKeepaliveLeaseLossIsNotClientDisconnect(t *testing.T) {
	c, _ := newPassthroughKeepaliveTestContext(t)
	ctx, release := WithAPIKeyAdmissionOwner(c.Request.Context())
	defer release()
	c.Request = c.Request.WithContext(ctx)
	owner, ok := apiKeyAdmissionOwnerFromContext(ctx)
	require.True(t, ok)
	owner.cancel(ErrAPIKeySlotLeaseLost)
	require.Error(t, ctx.Err())
	require.False(t, OpenAIDownstreamDisconnected(c))
}
