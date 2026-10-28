package vmkernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestClientDefaultUserAgentPreservesRuntimeInstallationIdentity(t *testing.T) {
	for _, transport := range []string{"http", "prefer_websocket"} {
		for _, owned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/owned=%t", transport, owned), func(t *testing.T) {
				b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
					var env struct {
						Options map[string]any
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
					if env.Options["user_agent"] != DefaultCodexUserAgent || env.Options["client_features"] != false || env.Options["analytics"] != false {
						t.Error("fixed Codex policy missing")
					}
					if env.Options["upstream_transport"] != transport {
						t.Error("transport policy changed")
					}
					if _, ok := env.Headers["x-codex-installation-id"]; ok {
						t.Error("installation header must not bypass options")
					}
					for _, key := range []string{"client_metadata", "user_agent", "options", "base_url", "authorization"} {
						if _, ok := env.Body[key]; ok {
							t.Errorf("caller identity survived: %s", key)
						}
					}
					var metadata map[string]any
					if err := json.Unmarshal(env.Body["metadata"], &metadata); err != nil {
						t.Fatal(err)
					}
					if len(metadata) != 1 || metadata["business"] != "keep" {
						t.Error("business metadata not preserved or identity leaked")
					}
					if owned {
						if env.Options["installation_id"] != "owned-device" {
							t.Error("runtime identity not enforced")
						}
					} else if _, ok := env.Options["installation_id"]; ok {
						t.Error("caller installation accepted")
					}
					if env.Session["session_id"] != "conversation-a" {
						t.Error("conversation changed")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
				})
				if owned {
					b.InstallationID = "owned-device"
				}
				client := NewClient()
				client.UpstreamTransport = transport
				defer client.Close()
				ctx := WithHeaders(context.Background(), http.Header{"User-Agent": {"caller-ua"}, "X-Codex-Installation-Id": {"caller-device"}})
				result, err := client.Execute(ctx, b, Request{Protocol: "responses", Body: json.RawMessage(`{"model":"fixture","base_url":"http://untrusted.invalid","authorization":"caller-secret","metadata":{"user_id":"caller","device_id":"caller","installation_id":"caller","business":"keep"},"prompt_cache_key":"conversation-a","user_agent":"caller-top-level","options":{"user_agent":"caller","installation_id":"caller"},"client_metadata":{"installation_id":"caller","x-codex-installation-id":"caller"}}`)}, nil)
				if err != nil || !result.Completed {
					t.Fatal("request failed", err)
				}
			})
		}
	}
}
