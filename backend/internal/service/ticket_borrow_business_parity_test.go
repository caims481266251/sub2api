package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type borrowParityHTTPUpstream struct {
	borrow *TicketBorrowService
	inner  *httpUpstreamRecorder
}

func (u *borrowParityHTTPUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxy, accountID, concurrency, nil)
}

func (u *borrowParityHTTPUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.borrow.ForwardHTTP(req, proxy, accountID, concurrency, profile, u.inner.DoWithTLS)
}

// Drive the real OAuth adapter, including stream=false aggregation and normal
// store normalization, then compare its wire request and usage with borrowing on.
func TestBorrowBusinessUsesNormalOAuthAdapter(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-5.6-sol", "gpt-6-sol"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", model, stream), func(t *testing.T) {
				borrow, repo := borrowFixture()
				borrow.repo = &borrowContinuationTestRepo{borrowTestRepo: repo, choice: &BorrowContinuationRoute{Route: repo.route}}
				upstream := &httpUpstreamRecorder{}
				gateway := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: borrow.owner.accountRepo,
					borrowRoutes: borrow, httpUpstream: &borrowParityHTTPUpstream{borrow: borrow, inner: upstream}}
				account, err := gateway.accountRepo.GetByID(t.Context(), 11)
				require.NoError(t, err)
				input := fmt.Sprintf(`{"model":%q,"reasoning":{"effort":"medium"},"stream":%t,"instructions":"preserve","input":[{"role":"user","content":"candy"}],"prompt_cache_key":"parity-session","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}`, model, stream)
				wire := fmt.Sprintf("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_parity\"}}\n\n"+
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n"+
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_parity\",\"model\":%q,\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n", model)
				var normalBody, normalOutput string
				var normalHeaders http.Header
				for _, enabled := range []bool{false, true} {
					repo.enabled = enabled
					upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(input))
					c.Request.Header.Set("Content-Type", "application/json")
					SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
					result, err := gateway.Forward(t.Context(), c, account, []byte(input))
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, 7, result.Usage.InputTokens)
					require.Equal(t, 3, result.Usage.OutputTokens)
					require.Equal(t, gjson.False, gjson.GetBytes(upstream.lastBody, "store").Type, "client omission keeps the ordinary OAuth store=false transform")
					require.Equal(t, gjson.True, gjson.GetBytes(upstream.lastBody, "stream").Type)
					require.Equal(t, model, gjson.GetBytes(upstream.lastBody, "model").String())
					require.Equal(t, "medium", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
					require.Contains(t, w.Body.String(), "21")
					headers := upstream.lastReq.Header.Clone()
					if !enabled {
						normalBody, normalHeaders, normalOutput = string(upstream.lastBody), headers, w.Body.String()
					} else {
						require.Contains(t, headers.Get("Cookie"), "__oailb=donor-cookie")
						headers.Del("Cookie")
						require.Equal(t, normalHeaders, headers)
						require.JSONEq(t, normalBody, string(upstream.lastBody))
						require.Equal(t, normalOutput, w.Body.String())
					}
					require.Zero(t, activeBorrowLeases(repo))
				}
			})
		}
	}
}

// Compare the final ordinary request with the same request admitted through a
// fresh borrowed route. Routing must not replace model, tools, input or stream.
func TestBorrowBusinessPreservesNormalRequest(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-5.6-sol", "gpt-6-sol"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				svc, base := borrowFixture()
				svc.repo = &borrowContinuationTestRepo{borrowTestRepo: base, choice: &BorrowContinuationRoute{Route: base.route}}
				body := `{"model":"` + model + `","reasoning":{"effort":"` + effort + `"},"stream":true,"instructions":"preserve instructions","input":[{"role":"user","content":[{"type":"input_text","text":"preserve input"}]}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}],"tool_choice":"auto","parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"normal-cache","client_metadata":{"session_id":"normal-session","other":"keep"}}`
				ctx, recovery := WithBorrowBusinessRecovery(t.Context(), nil)
				defer recovery.Close()
				ctx = WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAI)
				profile := &tlsfingerprint.Profile{Name: "normal-target-fingerprint"}
				var normalBody string
				var normalHeaders http.Header
				for _, enabled := range []bool{false, true} {
					base.enabled = enabled
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, strings.NewReader(body))
					require.NoError(t, err)
					req.Header = http.Header{"Authorization": {"Bearer target-secret"}, "Accept": {"text/event-stream"}, "Content-Type": {"application/json"}, "Session_id": {"normal-session"}, "Conversation_id": {"normal-conversation"}, "User-Agent": {"normal-target-ua"}, "X-Codex-Turn-State": {"normal-turn"}}
					resp, err := svc.ForwardHTTP(req, "", 11, 10, profile, func(sent *http.Request, proxy string, id int64, concurrency int, sentProfile *tlsfingerprint.Profile) (*http.Response, error) {
						require.Equal(t, enabled, recovery.Active(), "only borrowing activates the bounded recovery budget")
						require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(sent.Context()))
						require.Same(t, profile, sentProfile)
						require.Empty(t, proxy)
						require.EqualValues(t, 11, id)
						require.Equal(t, 10, concurrency)
						actual, err := io.ReadAll(sent.Body)
						require.NoError(t, err)
						replay, err := sent.GetBody()
						require.NoError(t, err)
						copy, err := io.ReadAll(replay)
						replay.Close()
						require.NoError(t, err)
						require.Equal(t, actual, copy)
						headers := sent.Header.Clone()
						if !enabled {
							normalBody, normalHeaders = string(actual), headers
						} else {
							cookie, err := sent.Cookie("__oailb")
							require.NoError(t, err)
							require.Equal(t, "donor-cookie", cookie.Value)
							headers.Del("Cookie")
							require.Equal(t, normalHeaders, headers)
							require.Equal(t, normalBody, string(actual))
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
					})
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
				}
			})
		}
	}
}

// The upstream deliberately withholds the second delta and terminal. Both the
// ordinary and borrowed paths must flush each earlier delta before releasing it.
func TestBorrowBusinessStreamsBeforeCompletionLikeNormalAccount(t *testing.T) {
	var ordinaryOutput string
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "borrowed"}[enabled], func(t *testing.T) {
			svc, base := borrowFixture()
			base.enabled = enabled
			svc.repo = &borrowContinuationTestRepo{borrowTestRepo: base, choice: &BorrowContinuationRoute{Route: base.route}}
			second, terminal := make(chan struct{}), make(chan struct{})
			var secondOnce, terminalOnce sync.Once
			allowSecond := func() { secondOnce.Do(func() { close(second) }) }
			allowTerminal := func() { terminalOnce.Do(func() { close(terminal) }) }
			defer allowSecond()
			defer allowTerminal()
			first := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"first\"}\n\n"
			middle := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"second\"}\n\n"
			last := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"
			reader := &stagedOpenAISSEReadCloser{segments: [][]byte{[]byte(first), []byte(middle), []byte(last)}, gates: []<-chan struct{}{nil, second, terminal}}
			req, err := http.NewRequestWithContext(WithHTTPUpstreamProfile(t.Context(), HTTPUpstreamProfileOpenAI), http.MethodPost, chatgptCodexURL, strings.NewReader(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`))
			require.NoError(t, err)
			resp, err := svc.ForwardHTTP(req, "", 11, 10, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
			})
			require.NoError(t, err)
			defer resp.Body.Close()
			recorder := newOpenAIResponseFlushRecorder()
			resultCh, errCh := runOpenAIResponseFlushTestAsync(recorder, resp.Body, config.GatewayConfig{StreamDataIntervalTimeout: 30})
			waitOpenAIResponseFlushCount(t, recorder, 1)
			got, _ := recorder.snapshot()
			require.Contains(t, got, `"delta":"first"`)
			require.NotContains(t, got, `"delta":"second"`)
			require.NotContains(t, got, "response.completed")
			allowSecond()
			waitOpenAIResponseFlushCount(t, recorder, 2)
			got, _ = recorder.snapshot()
			require.Contains(t, got, `"delta":"second"`)
			require.NotContains(t, got, "response.completed")
			allowTerminal()
			require.NoError(t, <-errCh)
			require.NotNil(t, <-resultCh)
			got, _ = recorder.snapshot()
			require.True(t, strings.HasPrefix(got, first+middle))
			require.Equal(t, 1, strings.Count(got, "response.completed"))
			if !enabled {
				ordinaryOutput = got
			} else {
				require.Equal(t, ordinaryOutput, got)
			}
			require.NoError(t, resp.Body.Close())
			require.Zero(t, activeBorrowLeases(base))
		})
	}
}

type slowBorrowBindingRepo struct {
	*borrowContinuationTestRepo
	started chan struct{}
	release chan struct{}
}

func (r *slowBorrowBindingRepo) BindHTTPContinuationResponse(ctx context.Context, id int64, response, route, session string) error {
	close(r.started)
	select {
	case <-r.release:
		return r.borrowContinuationTestRepo.BindHTTPContinuationResponse(ctx, id, response, route, session)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestBorrowBusinessSlowBindingDoesNotBlockText(t *testing.T) {
	svc, base := borrowFixture()
	repo := &slowBorrowBindingRepo{borrowContinuationTestRepo: &borrowContinuationTestRepo{borrowTestRepo: base, choice: &BorrowContinuationRoute{Route: base.route}}, started: make(chan struct{}), release: make(chan struct{})}
	svc.repo = repo
	payload := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_slow\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"visible before database\"}\n\n"
	resp, err := svc.ForwardHTTP(borrowRequest(t), "", 11, 10, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	var once sync.Once
	unblock := func() { once.Do(func() { close(repo.release) }) }
	defer unblock()
	read := make(chan string, 1)
	go func() { b := make([]byte, 4096); n, _ := resp.Body.Read(b); read <- string(b[:n]) }()
	select {
	case <-repo.started:
	case <-time.After(time.Second):
		t.Fatal("binding not started")
	}
	select {
	case got := <-read:
		require.Equal(t, payload, got)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("metadata binding blocked response bytes")
	}
	waited := make(chan error, 1)
	go func() { waited <- svc.waitResponseBinding(t.Context(), 11, borrowResponseKey("resp_slow")) }()
	select {
	case <-waited:
		t.Fatal("continuation passed before metadata was durable")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	require.NoError(t, <-waited)
	require.NoError(t, resp.Body.Close())
	require.Zero(t, activeBorrowLeases(base))
}

func TestBorrowBusinessCompletionWaitsForBindingAfterFlushingText(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "one-chunk", true: "split-terminal"}[split], func(t *testing.T) {
			svc, base := borrowFixture()
			repo := &slowBorrowBindingRepo{borrowContinuationTestRepo: &borrowContinuationTestRepo{borrowTestRepo: base, choice: &BorrowContinuationRoute{Route: base.route}}, started: make(chan struct{}), release: make(chan struct{})}
			svc.repo = repo
			prefix := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_durable\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"visible before database\"}\n\n"
			terminal := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_durable\"}}\n\n"
			segments := [][]byte{[]byte(prefix + terminal)}
			if split {
				segments = [][]byte{[]byte(prefix + terminal[:20]), []byte(terminal[20:])}
			}
			reader := &stagedOpenAISSEReadCloser{segments: segments}
			resp, err := svc.ForwardHTTP(borrowRequest(t), "", 11, 10, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}, nil
			})
			require.NoError(t, err)
			defer resp.Body.Close()
			unblock := sync.OnceFunc(func() { close(repo.release) })
			defer unblock()
			first := make(chan string, 1)
			go func() { p := make([]byte, 4096); n, _ := resp.Body.Read(p); first <- string(p[:n]) }()
			var got string
			select {
			case got = <-first:
				require.Contains(t, got, "visible before database")
			case <-time.After(200 * time.Millisecond):
				t.Fatal("completion binding held back earlier text in the same network chunk")
			}
			last := make(chan string, 1)
			go func() { p, _ := io.ReadAll(resp.Body); last <- string(p) }()
			select {
			case <-last:
				t.Fatal("completion escaped before its continuation binding was durable")
			case <-time.After(20 * time.Millisecond):
			}
			unblock()
			require.Equal(t, prefix+terminal, got+<-last, "observation must preserve every SSE byte")
			require.Equal(t, borrowResponseKey("resp_durable"), repo.boundResponse)
			require.Zero(t, activeBorrowLeases(base))
		})
	}
}
