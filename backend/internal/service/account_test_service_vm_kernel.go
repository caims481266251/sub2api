package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

// Management probes use the same guest boundary as public requests, while
// retaining the existing payloads, stream parser and quality/admission outcome
// handling. A disabled business scheduler is not permission to use host egress.
func (s *AccountTestService) testVMKernelConnection(c *gin.Context, account *Account, requestedModel, prompt string) error {
	runtime := LookupVMKernelService(account.Platform)
	if runtime == nil {
		return s.sendErrorAndEnd(c, "VM kernel runtime is unavailable")
	}
	model, err := vmKernelProbeModel(account, requestedModel)
	if err != nil {
		return s.sendErrorAndEnd(c, "Model is not allowed for this account")
	}
	upstreamModel := account.GetMappedModel(model)
	protocol, body, err := vmKernelAccountTestPayload(c, account, upstreamModel, prompt)
	if err != nil {
		return s.sendErrorAndEnd(c, "Invalid VM test request")
	}
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	// Only release a completed stream to the existing parser after the guest
	// has validated its terminal and usage. Incomplete execution must never
	// become a successful admission/quality sample. The bound is the same as
	// ordinary VM presentation; scheduled output retains its existing limits.
	var stream bytes.Buffer
	result, err := runtime.ExecuteProbe(c.Request.Context(), account, protocol, body, func(event vmkernel.Event) error {
		var data bytes.Buffer
		if err := json.Compact(&data, event.Data); err != nil {
			return err
		}
		if stream.Len()+data.Len()+8 > vmKernelPresentationLimit {
			return fmt.Errorf("VM test response exceeds presentation limit")
		}
		stream.WriteString("data: ")
		stream.Write(data.Bytes())
		stream.WriteString("\n\n")
		return nil
	})
	if account.Platform == PlatformOpenAI && s.openaiGatewayService != nil && !IsAstraSourceAcquisition(c.Request.Context()) {
		s.openaiGatewayService.observeVMKernelOpenAIResult(c.Request.Context(), c, account, upstreamModel, result)
	}
	if err != nil || result == nil || !result.Completed {
		// Preserve the original group-test cost observation for failed attempts;
		// partial output is never promoted to a successful quality answer.
		usageProtocol := "openai"
		if protocol == "messages" {
			usageProtocol = "anthropic"
		}
		recordPelicanTestSSE(c.Request.Context(), usageProtocol, upstreamModel, stream.Bytes())
		if result != nil && len(result.ErrorEvent) > 0 {
			if message := sanitizeUpstreamErrorMessage(extractOpenAISSEErrorMessage(result.ErrorEvent)); message != "" {
				return s.sendErrorAndEnd(c, message)
			}
		}
		return s.sendErrorAndEnd(c, "VM kernel test did not complete")
	}
	if protocol == "messages" {
		return s.processClaudeStream(c, &stream)
	}
	return s.processOpenAIStream(c, &stream)
}

func vmKernelAccountTestPayload(c *gin.Context, account *Account, model, prompt string) (string, []byte, error) {
	ctx := c.Request.Context()
	options, pelican := pelicanTestOptionsFromContext(ctx)
	protocol := "responses"
	var payload map[string]any
	var err error
	if account.Platform == PlatformAnthropic {
		protocol = "messages"
		if pelican {
			payload, err = createPelicanClaudePayload(model, options.prompt, options.reasoningEffort)
		} else {
			payload, err = createTestPayload(model)
		}
	} else if pelican {
		payload = createPelicanOpenAIPayload(model, true, options.prompt, options.reasoningEffort)
	} else {
		payload = createOpenAITestPayload(model, true)
	}
	if err != nil {
		return "", nil, err
	}
	applyModelTracePayload(ctx, payload)
	body, err := json.Marshal(payload)
	// Keep the VM connection test's existing explicit prompt support. Pelican
	// and admission questions always use their original dedicated payload.
	if err == nil && !pelican && strings.TrimSpace(prompt) != "" {
		path := "input.0.content.0.text"
		if protocol == "messages" {
			path = "messages.0.content.0.text"
		}
		body, err = sjson.SetBytes(body, path, strings.TrimSpace(prompt))
	}
	return protocol, body, err
}
