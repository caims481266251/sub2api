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

	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Exercise the real HTTP forwarding and Unix-socket kernel boundary, including
// owner publication after completion and the final runtime generation check.
func TestVMKernelHTTPContinuationUDSOwnerAndGeneration(t *testing.T) {
	var attempts atomic.Int32
	bodies := make(chan json.RawMessage, 4)
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		var envelope struct {
			Body json.RawMessage `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies <- envelope.Body
		attempt := attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_vm_http_history_%d\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.5\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer %d\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}\n\n", attempt, attempt)
	})
	svc := &OpenAIGatewayService{vmKernel: runtime}
	const userID, keyID, groupID = int64(301), int64(401), int64(501)
	t.Cleanup(func() {
		vmKernelContinuations.mu.Lock()
		defer vmKernelContinuations.mu.Unlock()
		for key, entry := range vmKernelContinuations.entries {
			if key.groupID == groupID && key.userID == userID && key.accountID == account.ID && strings.HasPrefix(key.responseID, "resp_vm_http_history_") {
				vmKernelContinuations.bytes -= entry.snapshot.size
				delete(vmKernelContinuations.entries, key)
			}
		}
		vmKernelContinuations.prune(time.Now())
	})
	forward := func(user, key, group int64, selected *Account, body string) (*httptest.ResponseRecorder, *OpenAIForwardResult, error) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Set("api_key", &APIKey{ID: key, GroupID: &group})
		SetOpenAIHTTPResponseOwner(c, user, key)
		result, err := svc.forwardVMKernelResponses(context.Background(), c, selected, []byte(body), "responses", "")
		return w, result, err
	}

	first, result, err := forward(userID, keyID, groupID, account, `{"model":"gpt-5.5","input":"first question","stream":false}`)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.VMKernelUsageAuthoritative)
	require.False(t, result.Stream)
	require.Equal(t, "resp_vm_http_history_1", gjson.Get(first.Body.String(), "id").String())
	<-bodies

	// The original owner rule allows another key belonging to the same user.
	continuation := `{"model":"gpt-5.5","previous_response_id":"resp_vm_http_history_1","input":"second question","stream":false}`
	second, result, err := forward(userID, keyID+1, groupID, account, continuation)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Stream, "guest replay stream=true must not change HTTP presentation")
	require.Equal(t, http.StatusOK, second.Code)
	require.Contains(t, second.Header().Get("Content-Type"), "application/json")
	require.True(t, gjson.Valid(second.Body.String()))
	require.Equal(t, "resp_vm_http_history_2", gjson.Get(second.Body.String(), "id").String())
	require.NotContains(t, second.Body.String(), "data:")
	replayed := <-bodies
	require.False(t, gjson.GetBytes(replayed, "previous_response_id").Exists())
	require.True(t, gjson.GetBytes(replayed, "stream").Bool(), "guest execution still streams for authoritative usage")
	require.Len(t, gjson.GetBytes(replayed, "input").Array(), 3)
	require.Contains(t, string(replayed), "first question")
	require.Contains(t, string(replayed), "answer 1")
	require.Contains(t, string(replayed), "second question")
	require.Equal(t, int32(2), attempts.Load())

	for _, foreign := range []struct {
		name             string
		user, key, group int64
	}{
		{"foreign user", userID + 1, keyID, groupID},
		{"foreign group", userID, keyID, groupID + 1},
	} {
		t.Run(foreign.name, func(t *testing.T) {
			w, out, err := forward(foreign.user, foreign.key, foreign.group, account, continuation)
			require.Error(t, err)
			require.Nil(t, out)
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Equal(t, int32(2), attempts.Load(), "foreign history must not reach the kernel")
		})
	}
	otherAccount := *account
	otherAccount.ID++
	_, _, err = forward(userID, keyID, groupID, &otherAccount, continuation)
	require.Error(t, err)
	require.Equal(t, int32(2), attempts.Load())

	// Simulate a healthy, reconciled replacement VM. Readiness and helper agree
	// on the new generation; the old history binding must still reject dispatch.
	cached, ok := vmKernelReadiness.Load(account.ID)
	require.True(t, ok)
	status := cached.(vmRuntimeStatus)
	status.state.Generation++
	status.state.GuestBootID = "boot-replacement"
	status.checked = time.Now()
	vmKernelReadiness.Store(account.ID, status)
	var inspections atomic.Int32
	helper := vmruntime.NewClient(vmForwardUDS(t, func(w http.ResponseWriter, r *http.Request) {
		var request vmruntime.Request
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Operation != "inspect" {
			t.Error("unexpected runtime operation")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		inspections.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"state": status.state})
	}))
	t.Cleanup(helper.CloseIdleConnections)
	runtime.helper = helper
	_, result, err = forward(userID, keyID+1, groupID, account, continuation)
	require.ErrorContains(t, err, "vm_session_generation_changed")
	require.Nil(t, result)
	require.Equal(t, int32(0), inspections.Load(), "admission uses the generation from the current control-plane observation without a per-request helper call")
	require.Equal(t, int32(2), attempts.Load(), "old history must not dispatch into a new guest generation")
}
