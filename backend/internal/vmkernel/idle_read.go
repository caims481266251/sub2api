package vmkernel

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

var errKernelStreamIdle = errors.New("kernel response stream read idle timeout")

// idleReadCloser arms a guard only while the HTTP body is being read. Event
// processing and downstream backpressure never consume the read allowance.
// Byte fragments and SSE comments count as transport progress independently of
// whether the parser can emit a public event. The request context, rather than
// the parent caller context, is canceled to interrupt a blocked network Read.
type idleReadCloser struct {
	io.ReadCloser
	timeout time.Duration
	ctx     context.Context
	cancel  context.CancelCauseFunc
}

func (r *idleReadCloser) Read(p []byte) (int, error) {
	var reading atomic.Bool
	reading.Store(true)
	done := make(chan struct{})
	timer := time.AfterFunc(r.timeout, func() {
		defer close(done)
		if reading.Swap(false) {
			r.cancel(errKernelStreamIdle)
		}
	})
	n, err := r.ReadCloser.Read(p)
	reading.Store(false)
	// A timer that raced with EOF must finish before the request returns and
	// its transport can be borrowed again. No delayed cancellation is retained.
	if !timer.Stop() {
		<-done
	}
	if errors.Is(context.Cause(r.ctx), errKernelStreamIdle) {
		return n, errKernelStreamIdle
	}
	return n, err
}
