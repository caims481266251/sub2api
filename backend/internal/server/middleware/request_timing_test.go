package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestRequestTimingCancellationUsesIngressContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, clientCanceled := range []bool{false, true} {
		name := "upstream_cleanup"
		if clientCanceled {
			name = "client_canceled_with_detached_handler"
		}
		t.Run(name, func(t *testing.T) {
			ingress, cancelIngress := context.WithCancel(context.Background())
			defer cancelIngress()
			router := gin.New()
			router.Use(RequestTiming())
			var got requesttiming.Snapshot
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				collector := requesttiming.From(c.Request.Context())
				collector.WhenFinished(func(s requesttiming.Snapshot) { got = s })
				if clientCanceled {
					c.Request = c.Request.WithContext(context.WithoutCancel(c.Request.Context()))
					cancelIngress()
				} else {
					upstream, cancelUpstream := context.WithCancel(c.Request.Context())
					c.Request = c.Request.WithContext(upstream)
					defer cancelUpstream()
				}
				c.String(http.StatusOK, "ok")
			})
			response := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ingress)
			router.ServeHTTP(response, req)
			if got.TraceID == "" || got.Canceled != clientCanceled || got.Status != http.StatusOK {
				t.Fatalf("cancellation must describe the ingress request: %+v", got)
			}
		})
	}
}

func TestRequestTimingPreReadAndHandlerRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestTiming())
	var got requesttiming.Snapshot
	r.Use(func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Fatal(err)
		}
		c.Request.Body = io.NopCloser(strings.NewReader(string(body)))
		c.Next()
	})
	r.POST("/v1/responses", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		requesttiming.From(c.Request.Context()).WhenFinished(func(s requesttiming.Snapshot) { got = s })
		c.String(200, "ok")
		c.Writer.Flush()
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("abc")))
	if got.BodyBytes != 3 || !got.BodyComplete || got.DownstreamBytes != 2 || got.Status != 200 {
		t.Fatalf("bad trace %+v", got)
	}
}

func TestRequestTimingHTTPGatewayCoverage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/v1/chat/completions", true},
		{http.MethodPost, "/openai/v1/chat/completions", true},
		{http.MethodPost, "/v1/alpha/search", true},
		{http.MethodPost, "/alpha/search", true},
		{http.MethodPost, "/v1/messages", true},
		{http.MethodPost, "/responses", true},
		{http.MethodPost, "/backend-api/codex/responses", true},
		{http.MethodPost, "/v1/responses/compact", true},
		{http.MethodGet, "/v1/responses", false}, // WebSocket owns its turn lifetime.
		{http.MethodGet, "/v1/models", false},
		{http.MethodPost, "/v1/embeddings", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			router := gin.New()
			router.Use(RequestTiming())
			var captured bool
			router.Handle(tc.method, tc.path, func(c *gin.Context) {
				collector := requesttiming.From(c.Request.Context())
				if (collector != nil) != tc.want {
					t.Fatalf("collector present = %v, want %v", collector != nil, tc.want)
				}
				body, err := io.ReadAll(c.Request.Body)
				if err != nil || string(body) != "abc" {
					t.Fatalf("request body changed: %q, %v", body, err)
				}
				if collector != nil {
					collector.WhenFinished(func(s requesttiming.Snapshot) {
						captured = true
						if s.BodyBytes != 3 || !s.BodyComplete || s.DownstreamBytes != 2 || s.Status != http.StatusCreated {
							t.Fatalf("incomplete HTTP trace: %+v", s)
						}
					})
				}
				c.String(http.StatusCreated, "ok")
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, strings.NewReader("abc")))
			if response.Code != http.StatusCreated || response.Body.String() != "ok" || captured != tc.want {
				t.Fatalf("response or timing changed: status=%d body=%q captured=%v", response.Code, response.Body.String(), captured)
			}
		})
	}
}

type timingFlushErrorRecorder struct {
	*httptest.ResponseRecorder
	err error
}

func (w *timingFlushErrorRecorder) FlushError() error { return w.err }

func TestRequestTimingRecordsTransportFlushFailureThroughAdmissionGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestTiming(), ServerTiming(true))
	var snapshot requesttiming.Snapshot
	want := errors.New("transport flush failed")
	r.POST("/v1/responses", func(c *gin.Context) {
		requesttiming.From(c.Request.Context()).WhenFinished(func(s requesttiming.Snapshot) { snapshot = s })
		c.Writer = &apiKeyAdmissionWriter{ResponseWriter: c.Writer, ctx: c.Request.Context()}
		_, err := c.Writer.WriteString("data: test\n\n")
		if err != nil {
			t.Fatal(err)
		}
		if err := service.FlushGatewayResponse(c.Writer); !errors.Is(err, want) {
			t.Fatalf("flush error = %v, want %v", err, want)
		}
	})
	recorder := &timingFlushErrorRecorder{ResponseRecorder: httptest.NewRecorder(), err: want}
	r.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if !snapshot.DownstreamError {
		t.Fatal("flush failure must be visible in request timing")
	}
	if _, ok := snapshot.Events["first_flush"]; ok {
		t.Fatal("failed flush recorded as successful")
	}
}
