package service

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
)

type vmKernelSSEUpdate struct {
	apply func() error
	ack   chan error
}

type vmKernelSSEOutcome struct {
	result *vmkernel.Result
	err    error
}

// Only this caller touches Gin and presentation state. The worker waits for each
// event/header callback to finish, preserving synchronous backpressure and errors.
func runVMKernelSSE(ctx context.Context, c *gin.Context, interval time.Duration,
	execute func(context.Context, func(vmkernel.Event) error) (*vmkernel.Result, error),
	headers func(http.Header), consume func(vmkernel.Event) error,
) (*vmkernel.Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	updates := make(chan vmKernelSSEUpdate)
	done := make(chan vmKernelSSEOutcome, 1)
	finished := make(chan struct{})
	dispatch := func(apply func() error) error {
		update := vmKernelSSEUpdate{apply: apply, ack: make(chan error, 1)}
		select {
		case updates <- update:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-update.ack:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx = vmkernel.WithOnResponseHeaders(ctx, func(h http.Header) {
		_ = dispatch(func() error { headers(h); return nil })
	})
	go func() {
		defer close(finished)
		outcome := vmKernelSSEOutcome{}
		defer func() {
			if recover() != nil {
				outcome.err = fmt.Errorf("kernel SSE execution panicked")
			}
			done <- outcome
		}()
		outcome.result, outcome.err = execute(ctx, func(event vmkernel.Event) error {
			return dispatch(func() error { return consume(event) })
		})
	}()
	defer func() { cancel(); <-finished }()

	// Cancellation must join the worker before Gin can recycle its context.
	join := func(cause error) (*vmkernel.Result, error) {
		var outcome vmKernelSSEOutcome
	wait:
		for {
			select {
			case outcome = <-done:
				break wait
			case update := <-updates:
				// A failed event is followed by trailer/header observation before
				// Execute returns. Acknowledge it without touching failed output.
				update.ack <- cause
			}
		}
		if OpenAIDownstreamDisconnected(c) && outcome.result != nil {
			outcome.result.ClientDisconnect = true
		}
		if outcome.err != nil {
			return outcome.result, outcome.err
		}
		return outcome.result, cause
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case outcome := <-done:
			return outcome.result, outcome.err
		case <-ctx.Done():
			return join(ctx.Err())
		case <-c.Request.Context().Done():
			cancel()
			return join(c.Request.Context().Err())
		case update := <-updates:
			before := c.Writer.Size()
			err := update.apply()
			update.ack <- err
			if c.Writer.Size() != before {
				timer.Reset(interval)
			}
			if err != nil {
				// The worker receives this exact callback error; canceling first
				// would misclassify a presentation failure as client cancellation.
				return join(err)
			}
		case <-timer.C:
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("X-Accel-Buffering", "no")
			c.Writer.Header().Del("Content-Length")
			n, err := c.Writer.Write([]byte(":\n\n"))
			recordOpenAIStreamKeepaliveBytes(c, n)
			if err == nil {
				err = FlushGatewayResponse(c.Writer)
			}
			if err != nil {
				c.Set(openAIDownstreamDisconnectedKey, true)
				cancel()
				return join(err)
			}
			timer.Reset(interval)
		}
	}
}
