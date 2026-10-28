package vmkernel

import "encoding/json"

// DefaultCodexUserAgent is the shared startup/request contract. The container
// isolation tests check its Python counterpart so release images cannot drift.
const DefaultCodexUserAgent = "codex_cli_rs/0.160.0 (linux x86_64)"

// stripCodexIdentity runs after the service has resolved session and turn
// provenance. Match vm2api's kernel boundary: caller identity is not provider
// input; owned installation identity travels in options, turn context in headers.
func stripCodexIdentity(body map[string]json.RawMessage) {
	for _, key := range []string{"base_url", "custom_base_url", "endpoint", "hostname", "api_key", "authorization", "client_metadata", "user_agent", "options"} {
		delete(body, key)
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(body["metadata"], &metadata) == nil && metadata != nil {
		for _, key := range []string{"user_id", "device_id", "installation_id"} {
			delete(metadata, key)
		}
		body["metadata"], _ = json.Marshal(metadata)
	}
}
