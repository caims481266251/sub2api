package vmkernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestClientRuntimeIdentityOverridesCaller(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				var env struct {
					Options map[string]string
					Headers map[string]string
					Body    map[string]json.RawMessage
					Session map[string]string
				}
				if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
					t.Error(err)
				}
				if env.Headers["user-agent"] != "" || env.Headers["User-Agent"] != "" {
					t.Error("caller UA header forwarded")
				}
				if owned {
					if env.Options["user_agent"] != "codex_cli_rs/0.164.0 (Mac OS 15.1; arm64)" || env.Options["installation_id"] != "owned-device" || env.Headers["x-codex-installation-id"] != "owned-device" {
						t.Error("runtime identity not enforced")
					}
					var metadata map[string]string
					json.Unmarshal(env.Body["client_metadata"], &metadata)
					if metadata["installation_id"] != "owned-device" || metadata["x-codex-installation-id"] != "owned-device" {
						t.Error("caller metadata survived")
					}
				} else if env.Options["user_agent"] != "" || env.Options["installation_id"] != "" {
					t.Error("legacy behavior changed")
				}
				if env.Session["session_id"] != "conversation-a" {
					t.Error("conversation changed")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			})
			if owned {
				b.UserAgent = "codex_cli_rs/0.164.0 (Mac OS 15.1; arm64)"
				b.InstallationID = "owned-device"
			}
			client := NewClient()
			defer client.Close()
			ctx := WithHeaders(context.Background(), http.Header{"User-Agent": {"caller-ua"}, "X-Codex-Installation-Id": {"caller-device"}})
			result, err := client.Execute(ctx, b, Request{Protocol: "responses", Body: json.RawMessage(`{"model":"fixture","prompt_cache_key":"conversation-a","options":{"user_agent":"caller"},"client_metadata":{"installation_id":"caller","x-codex-installation-id":"caller"}}`)}, nil)
			if err != nil || !result.Completed {
				t.Fatal("request failed", err)
			}
		})
	}
}

func TestClientRuntimeIdentityRejectsPartialBinding(t *testing.T) {
	for _, b := range []Binding{{UserAgent: "owned-ua"}, {InstallationID: "owned-device"}} {
		client := NewClient()
		result, err := client.Execute(context.Background(), b, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
		client.Close()
		if err == nil || result != nil {
			t.Fatal("partial identity admitted")
		}
	}
}
