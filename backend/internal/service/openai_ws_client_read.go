package service

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestcapture"
	coderws "github.com/coder/websocket"
)

// This marker is attached only at a read of the downstream client socket.
// An identical EOF from an upstream connection must remain an account failure.
type openAIWSClientReadDisconnectError struct{ err error }

func (e *openAIWSClientReadDisconnectError) Error() string { return e.err.Error() }
func (e *openAIWSClientReadDisconnectError) Unwrap() error { return e.err }

func markOpenAIWSClientReadDisconnect(err error) error {
	if err == nil || IsOpenAIWSClientReadDisconnect(err) || errors.Is(err, context.DeadlineExceeded) || !isOpenAIWSClientReadDisconnect(err) {
		return err
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return err
	}
	return &openAIWSClientReadDisconnectError{err: err}
}

// IsOpenAIWSClientReadDisconnect requires downstream read provenance. It does
// not classify a bare EOF, a local control close or an upstream read timeout.
func IsOpenAIWSClientReadDisconnect(err error) bool {
	var localClose *OpenAIWSClientCloseError
	if errors.As(err, &localClose) {
		return false
	}
	var disconnected *openAIWSClientReadDisconnectError
	return errors.As(err, &disconnected)
}

type openAIWSClientReadResult struct {
	messageType coderws.MessageType
	payload     []byte
	err         error
}

// ReadOpenAIWSClientMessage keeps one reader alive while control events send
// their close frame, then closes the transport and joins that reader.
func ReadOpenAIWSClientMessage(
	controlCtx context.Context,
	conn *coderws.Conn,
	timeout time.Duration,
	timeoutStatus coderws.StatusCode,
	timeoutReason string,
) (coderws.MessageType, []byte, error) {
	return readOpenAIWSClientMessageWithTimeoutStart(
		controlCtx,
		conn,
		timeout,
		timeoutStatus,
		timeoutReason,
		nil,
		nil,
	)
}

// readOpenAIWSClientMessageWithTimeoutStart supports readers whose timeout
// starts after a state transition, such as a completed passthrough turn. When
// timeoutActive is nil, a positive timeout starts immediately.
func readOpenAIWSClientMessageWithTimeoutStart(
	controlCtx context.Context,
	conn *coderws.Conn,
	timeout time.Duration,
	timeoutStatus coderws.StatusCode,
	timeoutReason string,
	timeoutStart <-chan struct{},
	timeoutActive func() bool,
) (coderws.MessageType, []byte, error) {
	if conn == nil {
		return 0, nil, errors.New("openai websocket client connection is nil")
	}
	if controlCtx == nil {
		controlCtx = context.Background()
	}

	readDone := make(chan openAIWSClientReadResult, 1)
	go func() {
		messageType, payload, err := conn.Read(context.Background())
		readDone <- openAIWSClientReadResult{messageType: messageType, payload: payload, err: markOpenAIWSClientReadDisconnect(err)}
	}()

	var timer *time.Timer
	var timeoutCh <-chan time.Time
	startTimeout := func() {
		if timeout <= 0 || (timeoutActive != nil && !timeoutActive()) {
			return
		}
		if timer == nil {
			timer = time.NewTimer(timeout)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		}
		timeoutCh = timer.C
	}
	if timeoutActive == nil || timeoutActive() {
		startTimeout()
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	closeAndJoin := func(status coderws.StatusCode, reason string, cause error) (coderws.MessageType, []byte, error) {
		_ = conn.Close(status, reason)
		_ = conn.CloseNow()
		<-readDone
		return 0, nil, NewOpenAIWSClientCloseError(status, reason, cause)
	}

	for {
		select {
		case result := <-readDone:
			if result.err == nil {
				capture := requestcapture.FromContext(controlCtx)
				capture.SetProtocol("websocket")
				capture.ClientFrame(result.payload)
			}
			return result.messageType, result.payload, result.err
		case <-timeoutStart:
			startTimeout()
		case <-timeoutCh:
			return closeAndJoin(timeoutStatus, timeoutReason, context.DeadlineExceeded)
		case <-controlCtx.Done():
			cause := context.Cause(controlCtx)
			if errors.Is(cause, ErrOpenAIWSIngressLeaseLost) || errors.Is(cause, ErrAPIKeySlotLeaseLost) {
				return closeAndJoin(
					coderws.StatusTryAgainLater,
					"websocket ingress capacity lease lost; please reconnect",
					cause,
				)
			}
			return closeAndJoin(coderws.StatusGoingAway, "websocket request canceled", cause)
		}
	}
}
