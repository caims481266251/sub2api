package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
)

// A management probe is still an account request. Use the same bound kernel
// and concurrency gate as connection tests, with no host transport fallback.
func probeDockerKernelOpenAI(ctx context.Context, account *Account, requestedModel string) (*vmkernel.Result, error) {
	runtime := LookupVMKernelService(account.Platform)
	if runtime == nil {
		return nil, fmt.Errorf("Docker kernel runtime is unavailable")
	}
	if requestedModel == "" && account.IsModelSupported("gpt-6.1-sol") {
		requestedModel = "gpt-6.1-sol"
	}
	model, err := vmKernelProbeModel(account, requestedModel)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(createOpenAITestPayload(account.GetMappedModel(model), true))
	if err != nil {
		return nil, err
	}
	result, err := runtime.ExecuteProbe(ctx, account, "responses", body, nil)
	if err != nil {
		return result, err
	}
	if result == nil || !result.Completed {
		return result, fmt.Errorf("Docker kernel probe did not complete")
	}
	return result, nil
}
