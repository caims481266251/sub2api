package vmkernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type poolConnectionID struct{}

func pooledKernel(t *testing.T, handler http.HandlerFunc) (Binding, *atomic.Int32, <-chan struct{}) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vmk-pool-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "k.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	connections := &atomic.Int32{}
	closed := make(chan struct{}, 128)
	server := &http.Server{
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, poolConnectionID{}, connections.Add(1))
		},
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				closed <- struct{}{}
			}
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/internal/v1/cancel" {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(204)
				return
			}
			handler(w, r)
		}),
	}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return Binding{Socket: path, Token: "fixture", InstanceID: "instance-a", Generation: 1, AccountID: 1, GuestBootID: "boot-a", CredentialRevision: "credential-a", ProxyRevision: "proxy-a"}, connections, closed
}

func completePoolRequest(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, "{\"id\":\"resp_pool\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}")
}

func executePoolRequest(client *Client, binding Binding) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := client.Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage("{}")}, nil)
	if err == nil && (result == nil || !result.Completed) {
		return errors.New("fixture did not complete")
	}
	return err
}

func TestConnectionPoolReusesSerialRequests(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			binding, connections, _ := pooledKernel(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Close {
					t.Error("request disabled keep-alive")
				}
				if !stream {
					completePoolRequest(w, r)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_pool\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			})
			client := NewClient()
			defer client.Close()
			for i := 0; i < 3; i++ {
				if err := executePoolRequest(client, binding); err != nil {
					t.Fatal(err)
				}
			}
			if got := connections.Load(); got != 1 {
				t.Fatalf("3 completed requests used %d connections", got)
			}
		})
	}
}

func TestConnectionPoolFencesIdentityChanges(t *testing.T) {
	changes := map[string]func(*Binding){
		"account":    func(b *Binding) { b.AccountID++ },
		"instance":   func(b *Binding) { b.InstanceID += "-new" },
		"generation": func(b *Binding) { b.Generation++ },
		"boot":       func(b *Binding) { b.GuestBootID += "-new" },
		"credential": func(b *Binding) { b.CredentialRevision += "-new" },
		"proxy":      func(b *Binding) { b.ProxyRevision += "-new" },
		"token":      func(b *Binding) { b.Token += "-new" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			binding, connections, closed := pooledKernel(t, completePoolRequest)
			client := NewClient()
			defer client.Close()
			if err := executePoolRequest(client, binding); err != nil {
				t.Fatal(err)
			}
			change(&binding)
			for i := 0; i < 2; i++ {
				if err := executePoolRequest(client, binding); err != nil {
					t.Fatal(err)
				}
			}
			if got := connections.Load(); got != 2 {
				t.Fatalf("identity fence used %d connections", got)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("old idle connection retained")
			}
		})
	}
}

func TestConnectionPoolDoesNotShareDifferentSockets(t *testing.T) {
	a, ca, _ := pooledKernel(t, completePoolRequest)
	b, cb, _ := pooledKernel(t, completePoolRequest)
	client := NewClient()
	defer client.Close()
	for i := 0; i < 3; i++ {
		for _, binding := range []Binding{a, b} {
			if err := executePoolRequest(client, binding); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ca.Load() != 1 || cb.Load() != 1 {
		t.Fatal("socket pools were not independent")
	}
}

func TestConnectionPoolDoesNotCapActiveConcurrency(t *testing.T) {
	const count = 40
	entered, release := make(chan struct{}, count+5), make(chan struct{})
	binding, connections, _ := pooledKernel(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		completePoolRequest(w, r)
	})
	client := NewClient()
	defer client.Close()
	done := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() { done <- executePoolRequest(client, binding) }()
	}
	allEntered := true
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
wait:
	for i := 0; i < count; i++ {
		select {
		case <-entered:
		case <-deadline.C:
			allEntered = false
			break wait
		}
	}
	close(release)
	for i := 0; i < count; i++ {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	if !allEntered {
		t.Fatal("idle pool limits blocked active concurrency")
	}
	for i := 0; i < 5; i++ {
		if err := executePoolRequest(client, binding); err != nil {
			t.Fatal(err)
		}
	}
	if got := connections.Load(); got != count {
		t.Fatalf("warm requests opened additional connections: %d", got)
	}
}

func TestConnectionPoolRetirementPreservesInFlight(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			binding, _, closed := pooledKernel(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.Context().Value(poolConnectionID{}).(int32) == 1 {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				completePoolRequest(w, r)
			})
			client := NewClient()
			defer client.Close()
			done := make(chan error, 1)
			go func() { done <- executePoolRequest(client, binding) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("request not started")
			}
			if shutdown {
				client.Close()
				if err := executePoolRequest(client, binding); err == nil {
					t.Error("closed client accepted request")
				}
			} else {
				binding.Generation++
				if err := executePoolRequest(client, binding); err != nil {
					t.Error(err)
				}
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("retirement interrupted in-flight stream: %v", err)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("retired connection retained after completion")
			}
		})
	}
}

func TestConnectionPoolFailedReusedRequestIsNotReplayed(t *testing.T) {
	var calls atomic.Int32
	binding, _, _ := pooledKernel(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		completePoolRequest(w, r)
	})
	client := NewClient()
	defer client.Close()
	if err := executePoolRequest(client, binding); err != nil {
		t.Fatal(err)
	}
	err := executePoolRequest(client, binding)
	var attempt *AttemptError
	if !errors.As(err, &attempt) || attempt.SafeToRetry || calls.Load() != 2 {
		t.Fatalf("replayed or marked dispatched request safe: calls=%d err=%v", calls.Load(), err)
	}
	if err := executePoolRequest(client, binding); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("next independent request did not recover")
	}
}

func TestConnectionPoolCancellationDoesNotPoisonNextRequest(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	binding, _, _ := pooledKernel(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 2 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
			w.(http.Flusher).Flush()
			close(entered)
			<-r.Context().Done()
			return
		}
		completePoolRequest(w, r)
	})
	client := NewClient()
	defer client.Close()
	if err := executePoolRequest(client, binding); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage("{}")}, nil)
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("second request not started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled request completed")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation stalled")
	}
	if err := executePoolRequest(client, binding); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("cancelled request replayed")
	}
}

type failedReuseWrite struct {
	net.Conn
	fail *atomic.Bool
}

func (c *failedReuseWrite) Write(p []byte) (int, error) {
	if c.fail.Load() {
		return 0, io.ErrClosedPipe
	}
	return c.Conn.Write(p)
}

func TestConnectionPoolZeroByteWriteDoesNotImplicitlyReplay(t *testing.T) {
	binding, _, _ := pooledKernel(t, completePoolRequest)
	client := NewClient()
	defer client.Close()
	_, release, err := client.acquireConnectionPool(binding)
	if err != nil {
		t.Fatal(err)
	}
	release()
	transport := client.pools[binding.Socket].transport
	dial := transport.DialContext
	var fail atomic.Bool
	var dials atomic.Int32
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err == nil && dials.Add(1) == 1 {
			return &failedReuseWrite{Conn: conn, fail: &fail}, nil
		}
		return conn, err
	}
	if err := executePoolRequest(client, binding); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	// The cancellation control call has its own transport and is not counted.
	err = executePoolRequest(client, binding)
	var attempt *AttemptError
	if !errors.As(err, &attempt) || attempt.SafeToRetry || dials.Load() != 1 {
		t.Fatalf("Transport retried a reused socket implicitly: dials=%d err=%v", dials.Load(), err)
	}
	if err := executePoolRequest(client, binding); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 2 {
		t.Fatal("independent request did not open a fresh connection")
	}
}

func TestConnectionPoolBoundedRetentionAndIdleExpiry(t *testing.T) {
	client := NewClient()
	defer client.Close()
	first := Binding{Socket: "/fixture/0", Token: "fixture", Generation: 1}
	_, release, err := client.acquireConnectionPool(first)
	if err != nil {
		t.Fatal(err)
	}
	release()
	old := client.pools[first.Socket]
	old.lastUsed = time.Now().Add(-2 * kernelIdleTimeout)
	_, release, err = client.acquireConnectionPool(first)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if !old.retired || client.pools[first.Socket] == old {
		t.Fatal("expired pool retained")
	}
	old = client.pools[first.Socket]
	for i := 1; i <= kernelPoolLimit; i++ {
		b := first
		b.Socket = fmt.Sprintf("/fixture/%d", i)
		_, release, err := client.acquireConnectionPool(b)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if len(client.pools) != kernelPoolLimit || !old.retired {
		t.Fatal("unbounded pool retention")
	}
	client.Close()
	if len(client.pools) != 0 {
		t.Fatal("shutdown retained pools")
	}
}

func TestClientUpstreamTransportPolicy(t *testing.T) {
	for _, policy := range []string{"", "http", "prefer_websocket", "invalid"} {
		t.Run(policy, func(t *testing.T) {
			var calls atomic.Int32
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var envelope map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
					t.Error(err)
				}
				var options map[string]any
				if err := json.Unmarshal(envelope["options"], &options); err != nil {
					t.Error(err)
				}
				want := policy
				if want == "" {
					want = "prefer_websocket"
				}
				if options["upstream_transport"] != want {
					t.Errorf("policy=%v want=%s", options, want)
				}
				completePoolRequest(w, r)
			})
			client := NewClient()
			defer client.Close()
			client.UpstreamTransport = policy
			err := executePoolRequest(client, binding)
			if policy == "invalid" {
				if err == nil || calls.Load() != 0 {
					t.Fatal("invalid policy dispatched")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
