package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Connection-local replay is supplemented by the bounded shared continuation
// store only after the original owner, account and VM binding are validated.
const vmKernelWSHistoryLimit = 16 << 20

type vmKernelWSSnapshot struct {
	input, output []json.RawMessage
	semantics     map[string]json.RawMessage
	size          int
}
type vmKernelWSHistory struct {
	snapshots map[string]vmKernelWSSnapshot
	order     []string
	bytes     int
}

func (h *vmKernelWSHistory) prepare(frame []byte) ([]byte, error) {
	if len(frame) > vmKernelWSHistoryLimit {
		return nil, fmt.Errorf("VM websocket frame too large")
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(frame, &req); err != nil {
		return nil, err
	}
	var kind string
	_ = json.Unmarshal(req["type"], &kind)
	if kind != "response.create" {
		return nil, fmt.Errorf("VM websocket expects response.create")
	}
	delete(req, "type")
	var previous string
	if raw := req["previous_response_id"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &previous); err != nil {
			return nil, err
		}
	}
	var input []json.RawMessage
	if raw := req["input"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &input); err != nil {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, fmt.Errorf("invalid VM websocket input")
			}
			item, _ := json.Marshal(map[string]any{"role": "user", "content": text})
			input = []json.RawMessage{item}
		}
	}
	if previous != "" {
		snapshot, ok := h.snapshots[previous]
		if !ok {
			return nil, fmt.Errorf("VM previous_response_id does not belong to this connection")
		}
		// A continuation carries input history, not an immutable request policy.
		// Explicit model/tool/instruction changes are checked by the normal turn
		// admission hooks. Previous instructions are intentionally not inherited.
		if _, exists := req["model"]; !exists {
			if model := snapshot.semantics["model"]; len(model) > 0 {
				req["model"] = model
			}
		}
		if _, exists := req["prompt_cache_key"]; !exists {
			if key := snapshot.semantics["prompt_cache_key"]; len(key) > 0 {
				req["prompt_cache_key"] = key
			}
		}
		merged := make([]json.RawMessage, 0, len(snapshot.input)+len(snapshot.output)+len(input))
		merged = append(merged, snapshot.input...)
		merged = append(merged, snapshot.output...)
		merged = append(merged, input...)
		input = merged
	}
	// The guest receives complete context. No upstream state lookup crosses a VM
	// generation, and continuation is only rebuilt from this connection's history.
	delete(req, "previous_response_id")
	raw, _ := json.Marshal(input)
	req["input"] = raw
	req["stream"] = json.RawMessage("true")
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(body) > vmKernelWSHistoryLimit {
		return nil, fmt.Errorf("VM websocket replay limit exceeded")
	}
	return body, nil
}

func (h *vmKernelWSHistory) complete(body []byte, response []byte) error {
	var req struct {
		Input []json.RawMessage `json:"input"`
	}
	var res struct {
		ID     string            `json:"id"`
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return err
	}
	if err := json.Unmarshal(response, &res); err != nil {
		return err
	}
	if res.ID == "" {
		return fmt.Errorf("VM websocket completed response missing id")
	}
	if len(body)+len(response) > vmKernelWSHistoryLimit {
		return fmt.Errorf("VM websocket replay limit exceeded")
	}
	if h.snapshots == nil {
		h.snapshots = make(map[string]vmKernelWSSnapshot)
	}
	if _, exists := h.snapshots[res.ID]; exists {
		return fmt.Errorf("VM duplicate response id")
	}
	semantics := map[string]json.RawMessage{}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	for _, key := range []string{"model", "instructions", "tools", "tool_choice", "parallel_tool_calls", "prompt_cache_key"} {
		if raw := fields[key]; len(raw) > 0 {
			semantics[key] = raw
		}
	}
	size := len(body) + len(response)
	for len(h.order) > 0 && (len(h.order) >= 32 || h.bytes+size > vmKernelWSHistoryLimit) {
		id := h.order[0]
		h.order = h.order[1:]
		h.bytes -= h.snapshots[id].size
		delete(h.snapshots, id)
	}
	h.snapshots[res.ID] = vmKernelWSSnapshot{req.Input, res.Output, semantics, size}
	h.order = append(h.order, res.ID)
	h.bytes += size
	return nil
}

// proxyVMKernelWebSocket uses the existing connection reader and admission /
// accounting hooks. The guest call is HTTP-over-UDS, not a second public socket.
func (s *OpenAIGatewayService) proxyVMKernelWebSocket(ctx context.Context, c *gin.Context, conn *coderws.Conn, account *Account, first []byte, hooks *OpenAIWSIngressHooks) error {
	if s.vmKernel == nil {
		return fmt.Errorf("VM kernel runtime unavailable")
	}
	reader := openAIWSGetIngressReader(c, conn)
	history := &vmKernelWSHistory{}
	ctx = vmkernel.WithSessionBinding(ctx, &vmkernel.SessionBinding{})
	fingerprint := openAITurnRouteFingerprint(account)
	frame := first
	turnState := ""
	turnStateSession := ""
	lastModel := gjson.GetBytes(first, "model").String()
	for turn := 1; ; turn++ {
		original := gjson.GetBytes(frame, "model").String()
		if original == "" {
			original = lastModel
			var err error
			frame, err = sjson.SetBytes(frame, "model", original)
			if err != nil {
				return err
			}
		}
		if hooks != nil && hooks.BeforePayloadParse != nil {
			if err := hooks.BeforePayloadParse(turn, frame, original); err != nil {
				return err
			}
		}
		if err := validateVMKernelIngress(account, frame); err != nil {
			return err
		}
		body, err := history.prepare(frame)
		if err != nil && gjson.GetBytes(frame, "previous_response_id").String() != "" {
			ctx, body, err = s.prepareVMKernelContinuation(ctx, c, account, frame)
		}
		if err != nil {
			return NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "VM continuation is unavailable or unauthorized", err)
		}
		frame, err = sjson.SetBytes(body, "type", "response.create")
		if err != nil {
			return err
		}
		frame, err = applyOpenAIWSReasoningEffortPolicy(frame, hooks)
		if err != nil {
			return err
		}
		if hooks != nil && hooks.BeforeRequest != nil {
			if err = hooks.BeforeRequest(turn, frame, original); err != nil {
				return err
			}
		}
		replayBody, _ := sjson.DeleteBytes(frame, "type")
		mapped := original
		if hooks != nil && hooks.MapRequestModel != nil {
			mapped, err = hooks.MapRequestModel(turn, original)
			if err != nil {
				return err
			}
		}
		if hooks != nil && hooks.BeforeTurn != nil {
			if err = hooks.BeforeTurn(turn); err != nil {
				return err
			}
		}
		started := time.Now()
		if hooks != nil && hooks.TurnStarted != nil {
			hooks.TurnStarted(turn, started)
		}
		latest, err := s.latestOpenAITurnAccount(ctx, c, account)
		if err == nil && (latest == nil || !latest.IsVMKernel() || openAITurnRouteFingerprint(latest) != fingerprint) {
			err = fmt.Errorf("VM websocket account binding changed")
		}
		if err != nil {
			if hooks != nil && hooks.AfterTurn != nil {
				hooks.AfterTurn(turn, nil, err)
			}
			return err
		}
		account = latest
		frame, err = sjson.SetBytes(frame, "model", mapped)
		if err != nil {
			if hooks != nil && hooks.AfterTurn != nil {
				hooks.AfterTurn(turn, nil, err)
			}
			return err
		}
		body, err = sjson.DeleteBytes(frame, "type")
		if err != nil {
			if hooks != nil && hooks.AfterTurn != nil {
				hooks.AfterTurn(turn, nil, err)
			}
			return err
		}
		prepared, billing, model, err := s.prepareVMKernelResponses(ctx, c, account, body, "responses", "")
		if err != nil {
			var policy *vmKernelRequestPolicyError
			var fast *OpenAIFastBlockedError
			var event []byte
			switch {
			case errors.As(err, &policy):
				event, _ = json.Marshal(map[string]any{"event_id": newOpenAIFastPolicyWSEventID(), "type": "error", "error": map[string]any{"type": policy.Type, "code": "policy_violation", "message": policy.Message}})
			case errors.As(err, &fast):
				event = buildOpenAIFastPolicyBlockedWSEvent(fast)
			default:
				event, _ = json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}})
			}
			if len(event) > 0 {
				_ = conn.Write(ctx, coderws.MessageText, event)
			}
			if hooks != nil && hooks.AfterTurn != nil {
				hooks.AfterTurn(turn, nil, err)
			}
			return err
		}
		if _, err = s.admitOpenAITurn(ctx, c, account, extractOpenAICodexTicketModel(prepared), prepared); err != nil {
			if hooks != nil && hooks.AfterTurn != nil {
				hooks.AfterTurn(turn, nil, err)
			}
			return err
		}
		session := vmKernelPreparedSession(prepared)
		if session != turnStateSession {
			turnState = ""
			turnStateSession = session
		}
		turnCtx, cancel := context.WithCancel(ctx)
		turnCtx = s.vmKernelResponsesTTFTContext(turnCtx)
		turnCtx = s.vmKernelResponsesHeaderContext(turnCtx, c, account, prepared, turnState)
		events := make(chan vmkernel.Event)
		type outcome struct {
			result *vmkernel.Result
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			r, e := s.vmKernel.Execute(turnCtx, account, "responses", prepared, func(event vmkernel.Event) error {
				select {
				case events <- event:
					return nil
				case <-turnCtx.Done():
					return turnCtx.Err()
				}
			})
			done <- outcome{r, e}
		}()
		var terminal []byte
		var terminalEvent []byte
		doneItems := newResponsesStreamOutputItems()
		doneBytes := 0
		requestedCancel := false
		var finished outcome
		var clientErr error
		providerErrorWritten := false
		active := true
		for active {
			select {
			case event := <-events:
				if event.Type == vmkernel.CodexMetadataEvent {
					if clientErr == nil {
						if err := conn.Write(ctx, coderws.MessageText, event.Data); err != nil {
							clientErr = err
							cancel()
						} else {
							s.noteVMKernelTurnState(c, account, prepared, vmKernelMetadataState(event.Data))
						}
					}
					continue
				}
				event.Data, err = restoreVMKernelResponsesTools(c, event.Data, event.Type)
				if err != nil {
					clientErr = err
					cancel()
					continue
				}
				if event.Type == "response.output_item.done" {
					doneBytes += len(event.Data)
					if doneBytes > vmKernelPresentationLimit {
						clientErr = fmt.Errorf("VM completed output exceeds limit")
						cancel()
						continue
					}
					doneItems.Observe(event.Data)
				}
				if gjson.GetBytes(event.Data, "type").String() == "response.completed" || gjson.GetBytes(event.Data, "type").String() == "response.done" {
					event.Data, _ = normalizeResponsesStreamingTerminalOutput(event.Data, nil, doneItems, nil)
					if len(event.Data) > vmKernelPresentationLimit {
						clientErr = fmt.Errorf("VM terminal event exceeds limit")
						cancel()
						continue
					}
					terminal = append([]byte(nil), gjson.GetBytes(event.Data, "response").Raw...)
					terminalEvent = append([]byte(nil), event.Data...)
					continue
				}
				if clientErr == nil {
					if err = conn.Write(ctx, coderws.MessageText, event.Data); err != nil {
						clientErr = err
						cancel()
					} else if event.Type == "error" || event.Type == "response.failed" || event.Type == "response.incomplete" {
						providerErrorWritten = true
					}
				}
			case incoming := <-reader.frames:
				if incoming.messageType != coderws.MessageText {
					clientErr = fmt.Errorf("VM websocket requires text frames")
					cancel()
					continue
				}
				kind := gjson.GetBytes(incoming.payload, "type").String()
				if kind == "response.cancel" {
					requestedCancel = true
					cancel()
				} else {
					clientErr = fmt.Errorf("overlapping VM response.create is not supported")
					cancel()
				}
			case <-reader.done:
				if clientErr == nil {
					clientErr = fmt.Errorf("VM websocket client disconnected")
					cancel()
				}
				// Avoid repeatedly selecting the closed reader channel while draining.
				finished = <-done
				active = false
			case finished = <-done:
				active = false
			case <-ctx.Done():
				cancel()
				finished = <-done
				active = false
			}
		}
		cancel()
		s.observeVMKernelOpenAIResult(ctx, c, account, model, finished.result)
		result, convertErr := vmKernelOpenAIResult(finished.result, prepared, original, billing, model, true)
		turnErr := vmKernelBusinessError(ctx, finished.result, finished.err)
		if turnErr == nil {
			turnErr = convertErr
		}
		if turnErr == nil {
			turnErr = clientErr
		}
		if result != nil {
			result.OpenAIWSMode = true
		}
		if turnErr == nil {
			if finished.result != nil {
				if state := extractOpenAICodexTurnState(finished.result.Headers); state != "" {
					turnState = state
				}
			}
			if len(terminal) == 0 && finished.result != nil {
				terminal = finished.result.Response
			}
			replayBody = vmKernelReplayCacheBody(c, account, replayBody, model)
			turnErr = history.complete(replayBody, terminal)
			if turnErr == nil {
				_ = s.rememberVMKernelContinuation(ctx, c, account, replayBody, terminal)
			}
		}
		if turnErr == nil && len(terminalEvent) > 0 {
			if finished.result != nil && finished.result.HasUsage {
				terminalEvent, err = sjson.SetRawBytes(terminalEvent, "response.usage", finished.result.Usage)
				if err != nil {
					turnErr = err
				}
			}
			if turnErr == nil {
				turnErr = conn.Write(ctx, coderws.MessageText, terminalEvent)
			}
		}
		if result != nil && turnErr != nil {
			result.streamReadIncomplete = true
		}
		recordVMKernelRecentRequest(withRecentRequestScope(ctx, account, codexAccountIdentitySource(c, account)), s.recentRequests, account, model, finished.result, turnErr)
		if hooks != nil && hooks.AfterTurn != nil {
			hooks.AfterTurn(turn, result, turnErr)
		}
		if turnErr != nil {
			// KernelClient withholds failed terminal events until the transport
			// settles. Deliver the provider failure through the upgraded socket,
			// without repeating a streamed error or the accounting hook above.
			if !providerErrorWritten && !requestedCancel && clientErr == nil && ctx.Err() == nil && finished.result != nil && len(finished.result.ErrorEvent) > 0 {
				failure := restoreCodexToolNamesFromSSEContext(c, finished.result.ErrorEvent, gjson.GetBytes(finished.result.ErrorEvent, "type").String())
				if writeErr := conn.Write(ctx, coderws.MessageText, failure); writeErr != nil {
					return writeErr
				}
			}
			if !requestedCancel || clientErr != nil || ctx.Err() != nil {
				return turnErr
			}
			errorFrame := []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"response_cancelled","message":"Response cancelled; reconnect to start a new response","reconnect_required":true}}`)
			if err = conn.Write(ctx, coderws.MessageText, errorFrame); err != nil {
				return err
			}
			// No attempt-bound provider drain ACK exists. End this WS session
			// so the caller explicitly starts its next response on a fresh
			// connection; independent attempts and the resident kernel stay live.
			_ = conn.Close(coderws.StatusPolicyViolation, "response cancelled; reconnect required")
			return turnErr
		}
		lastModel = original
		kind, next, readErr := reader.read(ctx)
		if readErr != nil {
			return readErr
		}
		if kind != coderws.MessageText {
			return fmt.Errorf("VM websocket requires text frames")
		}
		frame = next
	}
}
