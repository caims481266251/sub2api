package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestVMKernelAccountTestPayloadReusesPelicanOptions(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(withPelicanTestOptions(context.Background(), pelicanTestOptions{prompt: CandyPrompt, reasoningEffort: "high"}))
			model := "gpt-6-luna"
			if platform == PlatformAnthropic {
				model = "claude-sonnet-4-6"
			}
			protocol, raw, err := vmKernelAccountTestPayload(c, &Account{Platform: platform}, model, "ignored outside Pelican")
			require.NoError(t, err)
			var want map[string]any
			if platform == PlatformAnthropic {
				require.Equal(t, "messages", protocol)
				want, err = createPelicanClaudePayload(model, CandyPrompt, "high")
				require.NoError(t, err)
			} else {
				require.Equal(t, "responses", protocol)
				want = createPelicanOpenAIPayload(model, true, CandyPrompt, "high")
			}
			if platform == PlatformAnthropic {
				// The original Claude payload factory creates a fresh device and
				// session for every request; compare behavior without random IDs.
				metadata := gjson.GetBytes(raw, "metadata.user_id").String()
				require.NotEmpty(t, gjson.Get(metadata, "device_id").String())
				require.NotEmpty(t, gjson.Get(metadata, "session_id").String())
				want["metadata"] = map[string]any{"user_id": metadata}
			}
			encoded, err := json.Marshal(want)
			require.NoError(t, err)
			require.JSONEq(t, string(encoded), string(raw))
		})
	}
}

func TestVMKernelCandyProbeUsesGuestWhileBusinessAdmissionPending(t *testing.T) {
	requests := make(chan []byte, 1)
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		var envelope struct {
			Body json.RawMessage `json:"body"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&envelope))
		requests <- append([]byte(nil), envelope.Body...)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range vmForwardEvents("responses") {
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(event, "hello", "21"))
		}
	}, func(a *Account) { a.Schedulable = false })
	previous, loaded := vmKernelServices.Swap(PlatformOpenAI, runtime)
	t.Cleanup(func() {
		if loaded {
			vmKernelServices.Store(PlatformOpenAI, previous)
		} else {
			vmKernelServices.Delete(PlatformOpenAI)
		}
	})
	svc := &AccountTestService{accountRepo: vmForwardAccountRepo{account: account}}
	result, err := svc.RunPelicanBackground(context.Background(), account.ID, "gpt-6-luna", &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ReasoningEffort: "high", ParallelCount: 1})
	require.NoError(t, err)
	require.Equal(t, "success", result.Status, result.ErrorMessage)
	require.Equal(t, "21", result.ResponseText)
	request := <-requests
	require.Equal(t, "high", gjson.GetBytes(request, "reasoning.effort").String())
	require.Contains(t, string(request), "苹果")
	require.False(t, account.Schedulable, "a probe must not grant business admission")
}

func TestVMKernelQualityProbeDoesNotAcceptIncompleteKernelOutput(t *testing.T) {
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range vmForwardEvents("responses")[:4] {
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(event, "hello", "21"))
		}
	})
	previous, loaded := vmKernelServices.Swap(PlatformOpenAI, runtime)
	t.Cleanup(func() {
		if loaded {
			vmKernelServices.Store(PlatformOpenAI, previous)
		} else {
			vmKernelServices.Delete(PlatformOpenAI)
		}
	})
	svc := &AccountTestService{accountRepo: vmForwardAccountRepo{account: account}}
	result, err := svc.RunPelicanBackground(context.Background(), account.ID, "gpt-6-luna", &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ParallelCount: 1})
	require.NoError(t, err)
	require.Equal(t, "failed", result.Status)
	require.Empty(t, result.ResponseText)
}

func TestVMKernelClaudeQualityRetainsOriginalStopReasonVerdict(t *testing.T) {
	runtime, account := vmForwardFixture(t, PlatformAnthropic, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range vmForwardEvents("messages") {
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(event, "end_turn", "max_tokens"))
		}
	}, func(a *Account) { a.Credentials["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339) })
	previous, loaded := vmKernelServices.Swap(PlatformAnthropic, runtime)
	t.Cleanup(func() {
		if loaded {
			vmKernelServices.Store(PlatformAnthropic, previous)
		} else {
			vmKernelServices.Delete(PlatformAnthropic)
		}
	})
	svc := &AccountTestService{accountRepo: vmForwardAccountRepo{account: account}}
	result, err := svc.RunPelicanBackground(context.Background(), account.ID, "claude-sonnet-4-6", &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ParallelCount: 1})
	require.NoError(t, err)
	require.Equal(t, "failed", result.Status)
	require.Equal(t, pelicanErrMaxTokens, result.ErrorMessage)
}

func TestVMKernelAccountTestKeepsExplicitOrdinaryPrompt(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
		model, path := "gpt-6-luna", "input.0.content.0.text"
		if platform == PlatformAnthropic {
			model, path = "claude-sonnet-4-6", "messages.0.content.0.text"
		}
		_, body, err := vmKernelAccountTestPayload(c, &Account{Platform: platform}, model, "custom connection question")
		require.NoError(t, err)
		require.Equal(t, "custom connection question", gjson.GetBytes(body, path).String())
	}
}
