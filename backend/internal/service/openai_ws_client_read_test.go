package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestReadOpenAIWSClientMessage_ControlCloseFrames(t *testing.T) {
	tests := []struct {
		name          string
		timeout       time.Duration
		timeoutStatus coderws.StatusCode
		timeoutReason string
		cancelCause   error
		wantStatus    coderws.StatusCode
		wantReason    string
	}{
		{
			name:          "inter-turn idle sends normal close",
			timeout:       25 * time.Millisecond,
			timeoutStatus: coderws.StatusNormalClosure,
			timeoutReason: "websocket idle timeout",
			wantStatus:    coderws.StatusNormalClosure,
			wantReason:    "websocket idle timeout",
		},
		{
			name:          "first message timeout sends policy close",
			timeout:       25 * time.Millisecond,
			timeoutStatus: coderws.StatusPolicyViolation,
			timeoutReason: "missing first response.create message",
			wantStatus:    coderws.StatusPolicyViolation,
			wantReason:    "missing first response.create message",
		},
		{
			name:        "lease loss sends retry close",
			cancelCause: ErrOpenAIWSIngressLeaseLost,
			wantStatus:  coderws.StatusTryAgainLater,
			wantReason:  "websocket ingress capacity lease lost; please reconnect",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controlCtx, cancelControl := context.WithCancelCause(context.Background())
			defer cancelControl(context.Canceled)
			serverResult := make(chan error, 1)
			readStarted := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					serverResult <- err
					return
				}
				defer func() { _ = conn.CloseNow() }()
				close(readStarted)
				_, _, err = ReadOpenAIWSClientMessage(
					controlCtx,
					conn,
					tt.timeout,
					tt.timeoutStatus,
					tt.timeoutReason,
				)
				serverResult <- err
			}))
			defer server.Close()

			dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
			clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			cancelDial()
			require.NoError(t, err)
			defer func() { _ = clientConn.CloseNow() }()
			<-readStarted
			if tt.cancelCause != nil {
				cancelControl(tt.cancelCause)
			}

			readCtx, cancelRead := context.WithTimeout(context.Background(), time.Second)
			_, _, err = clientConn.Read(readCtx)
			cancelRead()
			var clientClose coderws.CloseError
			require.ErrorAs(t, err, &clientClose)
			require.Equal(t, tt.wantStatus, clientClose.Code)
			require.Equal(t, tt.wantReason, clientClose.Reason)

			select {
			case serverErr := <-serverResult:
				var closeErr *OpenAIWSClientCloseError
				require.ErrorAs(t, serverErr, &closeErr)
				require.Equal(t, tt.wantStatus, closeErr.StatusCode())
				require.Equal(t, tt.wantReason, closeErr.Reason())
				require.False(t, IsOpenAIWSClientReadDisconnect(serverErr), "local control closes must retain their original classification")
			case <-time.After(time.Second):
				t.Fatal("server read goroutine did not exit after close handshake")
			}
		})
	}
}

func TestReadOpenAIWSClientMessage_AbruptClientDisconnectHasReadProvenance(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared_ingress_reader=%t", shared), func(t *testing.T) {
			result := make(chan error, 1)
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					result <- err
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
				defer cancel()
				if shared {
					c, _ := gin.CreateTestContext(w)
					c.Request = r
					reader := openAIWSGetIngressReader(c, conn)
					close(started)
					_, _, err = reader.read(ctx)
				} else {
					close(started)
					_, _, err = ReadOpenAIWSClientMessage(ctx, conn, 0, 0, "")
				}
				result <- err
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer client.CloseNow()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("server reader did not start")
			}
			require.NoError(t, client.CloseNow()) // No close frame; reproduce the observed TCP EOF.
			select {
			case err := <-result:
				require.ErrorIs(t, err, io.EOF)
				require.True(t, IsOpenAIWSClientReadDisconnect(err))
				require.True(t, IsOpenAIWSClientReadDisconnect(fmt.Errorf("read next request: %w", err)))
			case <-ctx.Done():
				t.Fatal("client reader did not terminate after abrupt disconnect")
			}
		})
	}
}

func TestMarkOpenAIWSClientReadDisconnect_PreservesLocalControlAndTimeoutErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"deadline", context.DeadlineExceeded},
		{"deadline_with_eof", errors.Join(context.DeadlineExceeded, io.EOF)},
		{"socket_timeout", &net.OpError{Op: "read", Net: "tcp", Err: &net.DNSError{Err: "unexpected EOF", IsTimeout: true}}},
		{"local_policy", NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "policy", io.EOF)},
		{"local_idle", NewOpenAIWSClientCloseError(coderws.StatusNormalClosure, "idle timeout", context.DeadlineExceeded)},
		{"lease_lost", NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "lease lost", ErrOpenAIWSIngressLeaseLost)},
		{"local_shutdown", NewOpenAIWSClientCloseError(coderws.StatusGoingAway, "shutdown", context.Canceled)},
		{"local_wraps_client_disconnect", NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "policy", markOpenAIWSClientReadDisconnect(io.EOF))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marked := markOpenAIWSClientReadDisconnect(tt.err)
			require.Equal(t, tt.err, marked)
			require.False(t, IsOpenAIWSClientReadDisconnect(marked))
		})
	}
	// Without a read-boundary marker, an upstream EOF stays unclassified.
	require.False(t, IsOpenAIWSClientReadDisconnect(io.EOF))
	require.False(t, IsOpenAIWSClientReadDisconnect(io.ErrUnexpectedEOF))
}

func TestReadOpenAIWSClientMessage_ParentCancellationStillJoinsRead(t *testing.T) {
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	serverResult := make(chan error, 1)
	readStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverResult <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		close(readStarted)
		_, _, err = ReadOpenAIWSClientMessage(controlCtx, conn, 0, 0, "")
		serverResult <- err
	}))
	defer server.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()
	<-readStarted
	cancelControl(errors.New("server shutting down"))
	readCtx, cancelRead := context.WithTimeout(context.Background(), time.Second)
	_, _, err = clientConn.Read(readCtx)
	cancelRead()
	var clientClose coderws.CloseError
	require.ErrorAs(t, err, &clientClose)
	require.Equal(t, coderws.StatusGoingAway, clientClose.Code)
	require.Equal(t, "websocket request canceled", clientClose.Reason)

	select {
	case <-serverResult:
	case <-time.After(time.Second):
		t.Fatal("server read goroutine leaked after parent cancellation")
	}
}
