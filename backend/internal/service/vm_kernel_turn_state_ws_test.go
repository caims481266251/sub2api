package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestVMKernelTurnStateWSMetadataReconnectAndSessionChange(t *testing.T) {
	var attempts atomic.Int32
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		var env struct {
			Body    json.RawMessage
			Headers map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Error(err)
			return
		}
		want := map[int32]string{1: "", 2: "state-1", 3: "", 4: "state-3"}[n]
		if env.Headers[openAICodexTurnStateHeader] != want || gjson.GetBytes(env.Body, "client_metadata.x-codex-turn-state").String() != want {
			t.Errorf("turn %d state not synchronized: want %q", n, want)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		metadata, _ := json.Marshal(map[string]any{"type": "codex.response.metadata", "headers": map[string]string{"x-codex-turn-state": fmt.Sprintf("state-%d", n), "authorization": "private"}})
		fmt.Fprintf(w, "data: %s\n\n", metadata)
		for _, event := range vmForwardEvents("responses") {
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(event, "resp_fixture", fmt.Sprintf("resp_state_%d", n)))
		}
	})
	svc := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
	done := make(chan error, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer conn.CloseNow()
		_, first, err := conn.Read(r.Context())
		if err != nil {
			done <- err
			return
		}
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		c.Set("api_key", &APIKey{ID: 777})
		done <- svc.proxyVMKernelWebSocket(r.Context(), c, conn, account, first, nil)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := func() *coderws.Conn {
		conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
		require.NoError(t, err)
		return conn
	}
	call := func(conn *coderws.Conn, session, state string, n int) string {
		frame, _ := json.Marshal(map[string]any{"type": "response.create", "model": "gpt-5", "input": "hello", "prompt_cache_key": session, "client_metadata": map[string]string{"x-codex-turn-state": state}})
		require.NoError(t, conn.Write(ctx, coderws.MessageText, frame))
		observed := ""
		count := 0
		for {
			_, raw, err := conn.Read(ctx)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private")
			switch gjson.GetBytes(raw, "type").String() {
			case "codex.response.metadata":
				observed = vmKernelMetadataState(raw)
				count++
			case "response.completed":
				require.Equal(t, 1, count)
				require.Equal(t, fmt.Sprintf("state-%d", n), observed)
				return observed
			}
		}
	}
	closeConn := func(conn *coderws.Conn) {
		conn.CloseNow()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	conn := dial()
	call(conn, "session-a", "", 1)
	call(conn, "session-a", "stale-client-value", 2)
	state := call(conn, "session-b", "state-1", 3)
	closeConn(conn)
	conn = dial()
	call(conn, "session-b", state, 4)
	closeConn(conn)
	require.Equal(t, int32(4), attempts.Load())
}
