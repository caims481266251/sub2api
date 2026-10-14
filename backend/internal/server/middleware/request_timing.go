package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
)

// RequestTiming runs before authentication and body pre-read. WebSocket turns
// have a different lifetime and must not be labelled as an HTTP request trace.
func RequestTiming() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := strings.TrimRight(c.Request.URL.Path, "/")
		if c.Request.Method != http.MethodPost || !timedHTTPGatewayPath(path) {
			c.Next()
			return
		}
		// Handlers may replace the request context with an upstream timeout or
		// recovery context and cancel it during cleanup. Only the original
		// ingress context describes whether this HTTP request was canceled.
		ingressContext := c.Request.Context()
		collector := requesttiming.New(time.Now(), c.Request.ContentLength)
		c.Request = c.Request.WithContext(requesttiming.With(ingressContext, collector))
		c.Request.Body = collector.WrapBody(c.Request.Body)
		writer := &timingWriter{ResponseWriter: c.Writer, collector: collector}
		c.Writer = writer
		defer func() { collector.Finish(writer.Status(), ingressContext.Err() != nil) }()
		c.Next()
	}
}

func timedHTTPGatewayPath(path string) bool {
	for _, endpoint := range []string{"/responses", "/responses/compact", "/chat/completions", "/messages", "/alpha/search"} {
		if strings.HasSuffix(path, endpoint) {
			return true
		}
	}
	return false
}

type timingWriter struct {
	gin.ResponseWriter
	collector *requesttiming.Collector
}

func (w *timingWriter) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := w.ResponseWriter.Write(p)
	w.collector.Written(start, n, err, false)
	return n, err
}
func (w *timingWriter) WriteString(s string) (int, error) {
	start := time.Now()
	n, err := w.ResponseWriter.WriteString(s)
	w.collector.Written(start, n, err, false)
	return n, err
}
func (w *timingWriter) Flush() {
	start := time.Now()
	w.ResponseWriter.Flush()
	w.collector.Written(start, 0, nil, true)
}
func (w *timingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
