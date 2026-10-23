package service

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"testing"
)

func TestVMKernelResultKeepsProviderUsageAndIncompleteState(t *testing.T) {
	result := &vmkernel.Result{ResponseID: "response1", HasUsage: true, Completed: false, Terminal: "response.incomplete", Usage: json.RawMessage(`{"input_tokens":100,"output_tokens":4,"input_tokens_details":{"cached_tokens":80}}`)}
	out, err := vmKernelOpenAIResult(result, []byte(`{"service_tier":"priority"}`), "requested", "billing", "mapped", true)
	if err != nil {
		t.Fatal(err)
	}
	if out.Usage.InputTokens != 100 || out.Usage.CacheReadInputTokens != 80 {
		t.Fatalf("usage pre-subtracted: %+v", out.Usage)
	}
	if !out.VMKernelUsageAuthoritative || out.SucceededForScheduling() {
		t.Fatal("partial ownership or success incorrect")
	}
	if out.Model != "requested" || out.BillingModel != "billing" || out.UpstreamModel != "mapped" {
		t.Fatal("model attribution lost")
	}
}
func TestVMKernelResultZeroUsageRequiresPresenceAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		has  bool
		id   string
		want bool
	}{{false, "r", false}, {true, "", false}, {true, "r", true}} {
		out, err := vmKernelOpenAIResult(&vmkernel.Result{ResponseID: tc.id, HasUsage: tc.has, Usage: json.RawMessage(`{"input_tokens":0,"output_tokens":0}`)}, nil, "m", "m", "m", false)
		if err != nil {
			t.Fatal(err)
		}
		if out.VMKernelUsageAuthoritative != tc.want {
			t.Fatalf("authority mismatch: %+v", tc)
		}
	}
}
