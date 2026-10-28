package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
)

func TestVMKernelCancellationContractFollowsKernelPlatform(t *testing.T) {
	for _, tc := range []struct {
		name, platform, protocol, status string
		controlCalls                     int32
	}{
		{"codex", PlatformOpenAI, "responses", "stream_closed", 0},
		{"codex_cross_protocol", PlatformOpenAI, "messages", "stream_closed", 0},
		{"claude", PlatformAnthropic, "messages", "acknowledged", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var controls atomic.Int32
			socket := vmForwardUDS(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/cancel" {
					controls.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.protocol == "messages" {
					fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			client := vmkernel.NewClient()
			t.Cleanup(client.Close)
			s := &VMKernelService{kernel: client}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := &vmruntime.State{KernelSocket: socket, InternalToken: "fixture", InstanceID: "fixture", Generation: 1}
			result, err := s.call(ctx, state, &Account{ID: 1, Platform: tc.platform}, tc.protocol, []byte(`{"model":"fixture","input":"test","messages":[]}`), func(vmkernel.Event) error {
				cancel()
				return context.Canceled
			})
			if !errors.Is(err, context.Canceled) || result == nil || !result.ClientDisconnect || !result.ExecutionTransportCanceled {
				t.Fatalf("client cancellation lost: result=%+v err=%v", result, err)
			}
			if result.CancelStatus != tc.status || controls.Load() != tc.controlCalls || result.Settled || result.Completed {
				t.Fatalf("kernel cancellation contract changed: result=%+v controls=%d", result, controls.Load())
			}
		})
	}
}
