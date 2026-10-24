package service

import (
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// VM conversion owns presentation only. Native kernel usage never comes back
// from these converters into accounting (several converters synthesize zeros).
const vmKernelPresentationLimit = 32 << 20

func vmKernelConvertRequest(cfg *config.Config, ingress, provider string, body []byte) ([]byte, error) {
	if ingress == provider {
		return body, nil
	}
	var responses *apicompat.ResponsesRequest
	switch ingress {
	case "chat":
		var req apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		var err error
		responses, err = chatCompletionsToResponses(cfg, &req)
		if err != nil {
			return nil, err
		}
	case "messages":
		var req apicompat.AnthropicRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		var err error
		responses, err = anthropicToResponses(cfg, &req)
		if err != nil {
			return nil, err
		}
	case "responses":
		responses = &apicompat.ResponsesRequest{}
		if err := json.Unmarshal(body, responses); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported VM ingress %q", ingress)
	}
	if provider == "responses" {
		return json.Marshal(responses)
	}
	if provider != "messages" {
		return nil, fmt.Errorf("unsupported VM provider %q", provider)
	}
	req, err := apicompat.ResponsesToAnthropicRequest(responses)
	if err != nil {
		return nil, err
	}
	return json.Marshal(req)
}

// The caller only supplies standard, validated events. In particular a kernel
// terminal is withheld until the kernel tail has been validated by KernelClient.
// No synthetic finalize is run after a read error or cancellation.
type vmKernelPresentation struct {
	pending []struct {
		kind string
		data []byte
	}
	pendingBytes      int
	releasing         bool
	ingress, provider string
	stream            bool
	emit              func(string, []byte) error
	anthropic         *apicompat.AnthropicEventToResponsesState
	chat              *apicompat.ResponsesEventToChatState
	messages          *apicompat.ResponsesEventToAnthropicState
	accumulator       *apicompat.BufferedResponseAccumulator
	final             *apicompat.ResponsesResponse
	nativeResponse    json.RawMessage
	nativeDoneItems   *responsesStreamOutputItems
	native            *apicompat.AnthropicResponse
	nativeStopped     bool
	nativeInputs      map[int]string
	bytes             int
}

func newVMKernelPresentation(ingress, provider string, stream bool, emit func(string, []byte) error) *vmKernelPresentation {
	chat := apicompat.NewResponsesEventToChatState()
	// Match the original gateway: expose received usage for downstream billing
	// even when the client did not explicitly request stream_options.
	chat.IncludeUsage = true
	return &vmKernelPresentation{ingress: ingress, provider: provider, stream: stream, emit: emit,
		anthropic: apicompat.NewAnthropicEventToResponsesState(), chat: chat,
		messages: apicompat.NewResponsesEventToAnthropicState(), accumulator: apicompat.NewBufferedResponseAccumulator(),
		nativeDoneItems: newResponsesStreamOutputItems()}
}

func (p *vmKernelPresentation) event(kind string, data []byte) error {
	eventType := gjson.GetBytes(data, "type").String()
	terminal := eventType == "message_stop" || eventType == "response.completed" || eventType == "response.done" || (eventType == "message_delta" && gjson.GetBytes(data, "delta.stop_reason").String() != "")
	if terminal && !p.releasing {
		p.pendingBytes += len(data)
		if p.pendingBytes > vmKernelPresentationLimit || len(p.pending) >= 8 {
			return fmt.Errorf("VM terminal buffer limit exceeded")
		}
		p.pending = append(p.pending, struct {
			kind string
			data []byte
		}{kind, append([]byte(nil), data...)})
		return nil
	}

	if len(data) > vmKernelPresentationLimit {
		return fmt.Errorf("VM event exceeds presentation limit")
	}
	p.bytes += len(data)
	if p.bytes > vmKernelPresentationLimit {
		return fmt.Errorf("VM response exceeds presentation limit")
	}
	// Native Responses must preserve the provider's complete tool items,
	// including fields unknown to the cross-protocol presentation structs.
	if p.ingress == "responses" && p.provider == "responses" {
		p.nativeDoneItems.Observe(data)
		if terminal {
			data, _ = normalizeResponsesStreamingTerminalOutput(data, p.accumulator, p.nativeDoneItems, nil)
			p.nativeResponse = append(json.RawMessage(nil), gjson.GetBytes(data, "response").Raw...)
		}
		if p.stream {
			if responsesStreamEventMayContributeToOutput(eventType) {
				var event apicompat.ResponsesStreamEvent
				if err := json.Unmarshal(data, &event); err != nil {
					return err
				}
				p.accumulator.ProcessEvent(&event)
			}
			return p.emit(kind, data)
		}
		if terminal {
			return nil
		}
		if !responsesStreamEventMayContributeToOutput(eventType) {
			return nil
		}
	}
	// Native Messages streaming preserves signed thinking and native fields.
	if p.stream && p.ingress == p.provider {
		return p.emit(kind, data)
	}
	if p.provider == "messages" {
		var event apicompat.AnthropicStreamEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		if !p.stream && p.ingress == "messages" {
			return p.nativeMessageEvent(&event)
		}
		for _, converted := range apicompat.AnthropicEventToResponsesEvents(&event, p.anthropic) {
			if err := p.responseEvent(&converted); err != nil {
				return err
			}
		}
		return nil
	}
	var event apicompat.ResponsesStreamEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return err
	}
	return p.responseEvent(&event)
}

func (p *vmKernelPresentation) responseEvent(event *apicompat.ResponsesStreamEvent) error {
	if !p.stream {
		p.accumulator.ProcessEvent(event)
		if event.Response != nil && (event.Type == "response.completed" || event.Type == "response.done") {
			p.final = event.Response
		}
		return nil
	}
	emit := func(kind string, v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return p.emit(kind, data)
	}
	switch p.ingress {
	case "responses":
		return emit(event.Type, event)
	case "chat":
		for _, chunk := range apicompat.ResponsesEventToChatChunks(event, p.chat) {
			if err := emit("", chunk); err != nil {
				return err
			}
		}
	case "messages":
		for _, converted := range apicompat.ResponsesEventToAnthropicEvents(event, p.messages) {
			if err := emit(converted.Type, converted); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported VM presentation")
	}
	return nil
}

func (p *vmKernelPresentation) finish(model string, nativeUsage ...json.RawMessage) ([]byte, error) {
	p.releasing = true
	for _, event := range p.pending {
		data := event.data
		if len(nativeUsage) > 0 && len(nativeUsage[0]) > 0 {
			kind := gjson.GetBytes(data, "type").String()
			var err error
			if kind == "message_delta" {
				data, err = sjson.SetRawBytes(data, "usage", nativeUsage[0])
			}
			if kind == "response.completed" || kind == "response.done" {
				data, err = sjson.SetRawBytes(data, "response.usage", nativeUsage[0])
			}
			if err != nil {
				return nil, err
			}
		}
		if err := p.event(event.kind, data); err != nil {
			return nil, err
		}
	}
	p.pending = nil
	if p.native != nil && len(nativeUsage) > 0 {
		if err := json.Unmarshal(nativeUsage[0], &p.native.Usage); err != nil {
			return nil, err
		}
	}

	if p.stream {
		if p.ingress == "chat" {
			return nil, p.emit("", []byte("[DONE]"))
		}
		return nil, nil
	}
	if p.ingress == "messages" && p.provider == "messages" {
		if p.native == nil || !p.nativeStopped {
			return nil, fmt.Errorf("VM message has no validated stop")
		}
		return json.Marshal(p.native)
	}
	if p.ingress == "responses" && p.provider == "responses" && len(p.nativeResponse) > 0 {
		return ensureOpenAIResponseObjectCreatedAt(p.nativeResponse), nil
	}
	if p.final == nil {
		return nil, fmt.Errorf("VM response has no validated completed response")
	}
	p.accumulator.SupplementResponseOutput(p.final)
	switch p.ingress {
	case "responses":
		return json.Marshal(p.final)
	case "chat":
		return json.Marshal(apicompat.ResponsesToChatCompletions(p.final, model))
	case "messages":
		return json.Marshal(apicompat.ResponsesToAnthropic(p.final, model))
	default:
		return nil, fmt.Errorf("unsupported VM presentation")
	}
}

func (p *vmKernelPresentation) nativeMessageEvent(e *apicompat.AnthropicStreamEvent) error {
	switch e.Type {
	case "message_start":
		p.native = e.Message
		p.nativeInputs = make(map[int]string)
	case "message_delta":
		if p.native == nil {
			return fmt.Errorf("VM delta before message")
		}
		if e.Delta != nil && e.Delta.StopReason != "" {
			p.native.StopReason = apicompat.AnthropicStopReasonPtr(e.Delta.StopReason)
		}
		if e.Usage != nil {
			p.native.Usage.OutputTokens = e.Usage.OutputTokens
		}
	case "content_block_start":
		if p.native == nil || e.Index == nil || *e.Index != len(p.native.Content) || e.ContentBlock == nil {
			return fmt.Errorf("invalid VM content block index")
		}
		p.native.Content = append(p.native.Content, *e.ContentBlock)
	case "content_block_delta":
		if p.native == nil || e.Index == nil || *e.Index < 0 || *e.Index >= len(p.native.Content) || e.Delta == nil {
			return fmt.Errorf("invalid VM delta index")
		}
		b := &p.native.Content[*e.Index]
		switch e.Delta.Type {
		case "text_delta":
			b.Text += e.Delta.Text
		case "thinking_delta":
			b.Thinking += e.Delta.Thinking
		case "signature_delta":
			b.Signature += e.Delta.Signature
		case "input_json_delta":
			p.nativeInputs[*e.Index] += e.Delta.PartialJSON
		}
	case "content_block_stop":
		if e.Index != nil {
			if raw, ok := p.nativeInputs[*e.Index]; ok {
				if !json.Valid([]byte(raw)) {
					return fmt.Errorf("invalid VM tool arguments")
				}
				p.native.Content[*e.Index].Input = json.RawMessage(raw)
			}
		}
	case "message_stop":
		p.nativeStopped = true
	}
	return nil
}
