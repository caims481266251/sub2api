package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
)

type vmLifecycleTokens struct{}

func (vmLifecycleTokens) GetAccessToken(context.Context, *Account) (string, error) {
	return "token", nil
}

type vmLifecycleRepo struct {
	AccountRepository
	account *Account
	get     func() (*Account, error)
}

func (r *vmLifecycleRepo) GetByID(context.Context, int64) (*Account, error) {
	if r.get != nil {
		return r.get()
	}
	return r.account, nil
}

type vmLifecycleFixture struct {
	mu                        sync.Mutex
	state                     *vmruntime.State
	requests                  []vmruntime.Request
	inspectError              string
	inspectErrorWithState     bool
	kernelCalls               int
	kernelFail                bool
	kernelKnownFailure        bool
	kernelHTTPProviderFailure bool
	kernelEntered             chan struct{}
	kernelRelease             chan struct{}
}

func vmLifecycleSetup(t *testing.T) (*VMKernelService, *Account, *vmLifecycleFixture) {
	t.Helper()
	fixture := &vmLifecycleFixture{}
	proxyID := int64(445566)
	a := &Account{ID: 445566, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, ExecutionMode: AccountExecutionModeVMKernel, Credentials: map[string]any{"access_token": "token"}, ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Protocol: "http", Host: "proxy.invalid", Port: 8080, Status: StatusActive}}
	kernel := vmForwardUDS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/cancel" {
			return
		}
		fixture.mu.Lock()
		fixture.kernelCalls++
		fail, knownFailure := fixture.kernelFail, fixture.kernelKnownFailure
		httpProviderFailure := fixture.kernelHTTPProviderFailure
		entered, release := fixture.kernelEntered, fixture.kernelRelease
		fixture.mu.Unlock()
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		if release != nil {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		if httpProviderFailure {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"error":{"code":"provider_error","type":"provider_error","message":"fixture upstream rejection"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if fail {
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"truncated\"}\n\n")
			return
		}
		if knownFailure {
			fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"fixture-rejected\",\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"fixture\"}}}\n\n")
			return
		}
		for _, event := range vmForwardEvents("responses") {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	})
	helper := vmForwardUDS(t, func(w http.ResponseWriter, r *http.Request) {
		var request vmruntime.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		fixture.requests = append(fixture.requests, request)
		fault := func(code string) {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
		}
		if request.Operation == "inspect" {
			if fixture.inspectError != "" {
				if fixture.inspectErrorWithState {
					w.WriteHeader(409)
					json.NewEncoder(w).Encode(map[string]any{"state": fixture.state, "error": map[string]string{"code": fixture.inspectError}})
				} else {
					fault(fixture.inspectError)
				}
				return
			}
			if fixture.state == nil {
				fault("instance_not_found")
				return
			}
		}
		if request.Operation == "prepare" || request.Operation == "apply-config" {
			if request.Spec == nil || request.Projection == nil {
				fault("missing_projection")
				return
			}
			fixture.state = &vmruntime.State{AccountID: a.ID, InstanceID: "lifecycle-instance", Generation: request.Generation, GuestBootID: fmt.Sprintf("boot-%d", request.Generation), Spec: *request.Spec, Capacity: 1, Ready: true, Phase: "ready", KernelSocket: kernel, InternalToken: "fixture", AppliedCredentialRevision: request.Projection.Revision, AppliedProxyRevision: request.Spec.ProxyRevision}
		}
		if request.Operation == "remove" {
			fixture.state.Generation = request.Generation
			fixture.state.Ready = false
			fixture.state.DeleteRequested = true
			fixture.state.Phase = "removed"
			fixture.state.ContainerID = ""
		}
		if request.Operation == "stop" {
			fixture.state.Generation = request.Generation
			fixture.state.Ready = false
			fixture.state.Phase = "stopped"
		}
		json.NewEncoder(w).Encode(map[string]any{"state": fixture.state})
	})
	client := vmruntime.NewClient(helper)
	t.Cleanup(client.CloseIdleConnections)
	s := &VMKernelService{cfg: config.VMKernelConfig{Template: "default", CPU: 2, MemoryMiB: 2048}, repo: &vmLifecycleRepo{account: a}, platform: PlatformOpenAI, tokens: vmLifecycleTokens{}, helper: client, kernel: vmkernel.NewClient()}
	t.Cleanup(func() {
		vmKernelReadiness.Delete(a.ID)
		vmKernelHealth.Delete(a.ID)
		vmKernelServices.CompareAndDelete(a.Platform, s)
	})
	return s, a, fixture
}
func (f *vmLifecycleFixture) operations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.requests))
	for i, r := range f.requests {
		out[i] = fmt.Sprintf("%s:%d", r.Operation, r.Generation)
	}
	return out
}
func TestVMKernelLifecycleInitialAndRotation(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	if !VMKernelRuntimeReady(a) || f.kernelCalls != 0 || fmt.Sprint(f.operations()) != "[inspect:0 prepare:1]" {
		t.Fatalf("initial operations=%v probes=%d ready=%v", f.operations(), f.kernelCalls, VMKernelRuntimeReady(a))
	}
	s.prepare(context.Background(), a)
	if f.kernelCalls != 0 {
		t.Fatal("unchanged binding probed again")
	}
	a.Credentials["access_token"] = "rotated"
	s.prepare(context.Background(), a)
	if !VMKernelRuntimeReady(a) || f.kernelCalls != 0 || f.state.Generation != 2 || f.operations()[len(f.operations())-1] != "apply-config:2" {
		t.Fatalf("rotation operations=%v probes=%d state=%+v", f.operations(), f.kernelCalls, f.state)
	}
	s.slot(a.ID).verifiedBinding = ""
	s.prepare(context.Background(), a)
	if f.state.Generation != 2 || f.kernelCalls != 0 || f.operations()[len(f.operations())-1] != "inspect:0" {
		t.Fatalf("new coordinator failed operations=%v probes=%d", f.operations(), f.kernelCalls)
	}
}
func TestVMKernelLifecycleInspectErrorNeverCreates(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	f.inspectError = "helper_transport_unknown"
	s.prepare(context.Background(), a)
	if fmt.Sprint(f.operations()) != "[inspect:0]" || f.kernelCalls != 0 || VMKernelRuntimeReady(a) {
		t.Fatalf("unsafe create %v", f.operations())
	}
}
func TestVMKernelLifecycleBusinessPauseAllowsManagementProbe(t *testing.T) {
	for _, pause := range []string{"manual", "cooldown", "rate_limit", "overload"} {
		t.Run(pause, func(t *testing.T) {
			s, a, f := vmLifecycleSetup(t)
			until := time.Now().Add(time.Hour)
			switch pause {
			case "manual":
				a.Schedulable = false
			case "cooldown":
				a.TempUnschedulableUntil = &until
			case "rate_limit":
				a.RateLimitResetAt = &until
			case "overload":
				a.OverloadUntil = &until
			}
			s.prepare(context.Background(), a)
			if !VMKernelRuntimeReady(a) || a.IsSchedulable() {
				t.Fatal("business pause must not prevent runtime preparation or admit traffic")
			}
			if r, err := s.ExecuteProbe(context.Background(), a, "responses", []byte(`{}`), nil); err != nil || r == nil || !r.Completed {
				t.Fatalf("management probe rejected: %v", err)
			}
			calls := f.kernelCalls
			if _, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil); err == nil || f.kernelCalls != calls {
				t.Fatal("business pause bypassed")
			}
			s.prepare(context.Background(), a)
			if f.state.Generation != 1 || !f.state.Ready {
				t.Fatal("business pause restarted runtime")
			}
		})
	}
}

func TestVMKernelLifecycleInactiveRejectsProbe(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	a.Status = StatusDisabled
	if r, err := s.ExecuteProbe(context.Background(), a, "responses", []byte(`{}`), nil); err == nil || r != nil || len(f.operations()) != 0 {
		t.Fatalf("disabled account probed: %v", err)
	}
}

func TestVMKernelLifecycleUnknownRequiresNewGeneration(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	// Exercise actual Execute -> publish -> sanitized Summary, not a manually
	// constructed status that could hide an error-code projection mismatch.
	f.state.ActualAccelerator = "docker"
	s.publishHealth(a.ID, f.state, nil)
	s.publish(a, f.state, nil)
	f.kernelFail = true
	r, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil)
	if err == nil || r == nil || !r.Dispatched || VMKernelRuntimeReady(a) || !s.slot(a.ID).quarantined {
		t.Fatalf("unknown not quarantined %+v %v", r, err)
	}
	value, found := vmKernelReadiness.Load(a.ID)
	if !found || value.(vmRuntimeStatus).state.ErrorCode != "kernel_attempt_unknown" {
		t.Fatal("unknown attempt lost its controlled internal classification")
	}
	summary := VMKernelRuntimeSummary(a)
	if summary == nil || !summary.Ready || summary.ErrorCode != "" || summary.ActualAccelerator != "docker" {
		t.Fatalf("request outcome was displayed as a local runtime fault: %+v", summary)
	}
	f.kernelFail = false
	s.prepare(context.Background(), a)
	if f.state.Generation != 2 || !VMKernelRuntimeReady(a) || s.slot(a.ID).quarantined {
		t.Fatalf("binding not reprovisioned %+v %v", f.state, f.operations())
	}
}
func TestVMKernelLifecycleFinalCredentialAndGroupFence(t *testing.T) {
	for _, change := range []string{"credentials", "groups", "proxy"} {
		t.Run(change, func(t *testing.T) {
			s, a, f := vmLifecycleSetup(t)
			s.prepare(context.Background(), a)
			calls := 0
			repo := s.repo.(*vmLifecycleRepo)
			repo.get = func() (*Account, error) {
				calls++
				if calls < 2 {
					return a, nil
				}
				copy := *a
				if change == "credentials" {
					copy.Credentials = map[string]any{"access_token": "changed"}
				} else if change == "proxy" {
					proxy := *a.Proxy
					proxy.Host = "changed.invalid"
					copy.Proxy = &proxy
				} else {
					copy.GroupIDs = []int64{999}
				}
				return &copy, nil
			}
			_, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil)
			if err == nil || f.kernelCalls != 0 {
				t.Fatalf("final fence failed %v calls=%d", err, f.kernelCalls)
			}
		})
	}
}
func TestVMKernelLifecycleCapacityAndDeletionDrain(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	f.kernelEntered = make(chan struct{}, 2)
	f.kernelRelease = make(chan struct{})
	finished := make(chan error, 2)
	go func() { _, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil); finished <- err }()
	select {
	case <-f.kernelEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("kernel did not start")
	}
	if !VMKernelRuntimeReady(a) {
		t.Fatal("active inference incorrectly marks a healthy runtime unavailable")
	}
	go func() { _, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil); finished <- err }()
	select {
	case <-f.kernelEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("second request was serialized by runtime")
	}
	vmKernelServices.Store(a.Platform, s)
	deleted := make(chan error, 1)
	go func() { deleted <- PrepareVMKernelAccountDeletion(context.Background(), a) }()
	deadline := time.Now().Add(time.Second)
	for {
		slot := s.slot(a.ID)
		slot.mu.Lock()
		deleting := slot.deleting
		slot.mu.Unlock()
		if deleting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delete did not mark intent")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-deleted:
		t.Fatalf("deletion did not drain %v", err)
	default:
	}
	close(f.kernelRelease)
	for i := 0; i < 2; i++ {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if f.state.Phase != "removed" || !f.state.DeleteRequested || VMKernelRuntimeReady(a) {
		t.Fatalf("missing tombstone %+v", f.state)
	}
	if _, err := s.ExecuteProbe(context.Background(), a, "responses", []byte(`{}`), nil); err == nil {
		t.Fatal("deleted guest probed")
	}
}

func TestVMKernelCredentialProjectionCodexCanonicalSchema(t *testing.T) {
	a := &Account{ID: 42, Platform: PlatformOpenAI, Credentials: map[string]any{
		"access_token": "fixture-access", "refresh_token": "fixture-refresh",
		"chatgpt_account_id": "fixture-account", "email": "fixture@example.invalid",
		"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}}
	raw, err := vmCredentialProjection(a)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string][]map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope) != 1 || len(envelope["accounts"]) != 1 {
		t.Fatal("expected a single accounts projection")
	}
	row := envelope["accounts"][0]
	expected := map[string]string{"id": "42", "access_token": "fixture-access", "chatgpt_account_id": "fixture-account", "email": "fixture@example.invalid"}
	if len(row) != len(expected)+2 {
		t.Fatal("unexpected projected credential fields")
	}
	for key, value := range expected {
		if row[key] != value {
			t.Errorf("incorrect projected field %s", key)
		}
	}
	for _, key := range []string{"rpm", "max_concurrency"} {
		if value, exists := row[key]; !exists || value != float64(0) {
			t.Errorf("kernel must not add a second admission limit: %s=%v", key, value)
		}
	}
	for _, key := range []string{"account_id", "refresh_token", "refreshToken"} {
		if _, exists := row[key]; exists {
			t.Errorf("forbidden projected field %s", key)
		}
	}
}

func TestVMKernelCredentialProjectionRejectsInvalidCredentials(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		for _, tc := range []struct {
			name        string
			credentials map[string]any
			want        string
		}{
			{"missing_access", map[string]any{"refresh_token": "fixture-refresh"}, "vm_access_token_missing"},
			{"expired_access", map[string]any{"access_token": "fixture-access", "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}, "vm_access_token_expired"},
		} {
			t.Run(platform+"/"+tc.name, func(t *testing.T) {
				raw, err := vmCredentialProjection(&Account{Platform: platform, Credentials: tc.credentials})
				if err == nil || err.Error() != tc.want || len(raw) != 0 {
					t.Fatalf("expected rejection %s without projection", tc.want)
				}
			})
		}
	}
	t.Run("claude_missing_expiry", func(t *testing.T) {
		raw, err := vmCredentialProjection(&Account{Platform: PlatformAnthropic, Credentials: map[string]any{"access_token": "fixture-access"}})
		if err == nil || err.Error() != "vm_access_token_expiry_missing" || len(raw) != 0 {
			t.Fatal("expected missing expiry rejection without projection")
		}
	})
}

func TestVMKernelCredentialProjectionClaudeExcludesRefresh(t *testing.T) {
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	raw, err := vmCredentialProjection(&Account{Platform: PlatformAnthropic, Credentials: map[string]any{
		"access_token": "fixture-access", "refresh_token": "fixture-refresh", "expires_at": expires.Format(time.RFC3339), "scopes": "user:inference user:profile",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	row := envelope["claudeAiOauth"]
	if len(envelope) != 1 || len(row) != 3 || row["accessToken"] != "fixture-access" || row["expiresAt"] != float64(expires.UnixMilli()) {
		t.Fatal("unexpected Claude projection schema")
	}
	scopes, ok := row["scopes"].([]any)
	if !ok || len(scopes) != 2 || scopes[0] != "user:inference" || scopes[1] != "user:profile" {
		t.Fatal("incorrect projected scopes")
	}
	for _, key := range []string{"refresh_token", "refreshToken"} {
		if _, exists := row[key]; exists {
			t.Errorf("forbidden projected field %s", key)
		}
	}
}

func TestVMKernelHealthyRefreshBypassesSaturatedBootstrapLane(t *testing.T) {
	s, a, _ := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	if !VMKernelRuntimeReady(a) {
		t.Fatal("fixture did not become ready")
	}
	s.controlLimits()
	for i := 0; i < cap(s.bootstrapLimit); i++ {
		s.bootstrapLimit <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(s.bootstrapLimit); i++ {
			<-s.bootstrapLimit
		}
	}()
	value, _ := vmKernelReadiness.Load(a.ID)
	old := value.(vmRuntimeStatus)
	old.checked = time.Now().Add(-time.Minute)
	vmKernelReadiness.Store(a.ID, old)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	if !s.enqueue(ctx, a.ID, func(task context.Context) { s.prepare(task, a); close(done) }) {
		t.Fatal("enqueue failed")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("healthy inspection waited for cold bootstrap capacity")
	}
	s.workers.Wait()
	if !VMKernelRuntimeReady(a) {
		t.Fatal("healthy readiness was not refreshed while bootstraps occupied all slots")
	}
}

func TestVMKernelBackgroundOwnershipDeadlineAndStopDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &VMKernelService{cancel: cancel, done: make(chan struct{})}
	go func() { <-ctx.Done(); close(s.done) }()
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	if !s.enqueue(ctx, 1, func(task context.Context) {
		deadline, ok := task.Deadline()
		if !ok || time.Until(deadline) < 9*time.Minute {
			t.Error("bootstrap lacks independent ten minute deadline")
		}
		close(entered)
		<-task.Done()
		close(cancelled)
		<-release
	}) {
		t.Fatal("enqueue failed")
	}
	<-entered
	if s.enqueue(ctx, 1, func(context.Context) { t.Error("duplicate account task") }) {
		t.Fatal("duplicate account queued")
	}
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel background task")
	}
	if s.enqueue(context.Background(), 2, func(context.Context) { t.Error("work admitted during Stop") }) {
		t.Fatal("Stop permitted new work")
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned before in-flight task drained")
	default:
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not finish after drain")
	}
}

func TestVMKernelAdmissionWaitsForHealthyControlObservation(t *testing.T) {
	s, a, _ := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	slot := s.slot(a.ID)
	slot.mu.Lock() // A healthy reconcile holds this lock while inspecting helper.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Execute(ctx, a, "responses", []byte(`{"model":"gpt-5","input":"OK"}`), nil)
		done <- err
	}()
	select {
	case err := <-done:
		slot.mu.Unlock()
		t.Fatalf("healthy control observation produced early admission failure: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	slot.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("admission failed to resume")
	}
}

func TestVMKernelAdmissionControlWaitHonorsCancellation(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	slot := s.slot(a.ID)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Execute(ctx, a, "responses", []byte(`{}`), nil)
	if err != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kernelCalls != 0 {
		t.Fatal("cancelled admission dispatched an inference")
	}
}

func TestVMKernelTransientObservationRecoveryBound(t *testing.T) {
	for _, scenario := range []string{"recovers", "persistent", "quarantined", "mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			s, a, f := vmLifecycleSetup(t)
			ctx := context.Background()
			s.prepare(ctx, a)
			f.mu.Lock()
			generation := f.state.Generation
			f.state.Ready = false
			f.inspectError = "docker_operation_unknown"
			f.inspectErrorWithState = true
			f.mu.Unlock()
			if scenario == "quarantined" {
				s.slot(a.ID).quarantined = true
			}
			if scenario == "mismatch" {
				f.mu.Lock()
				f.inspectError = "guest_generation_mismatch"
				f.mu.Unlock()
			}
			s.prepare(ctx, a)
			if scenario == "quarantined" || scenario == "mismatch" {
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.state.Generation <= generation {
					t.Fatal("unsafe failure recovery delayed")
				}
				return
			}
			if VMKernelRuntimeReady(a) {
				t.Fatal("failed observation remained schedulable")
			}
			f.mu.Lock()
			if f.state.Generation != generation {
				t.Fatal("single transient failure restarted guest")
			}
			f.mu.Unlock()
			if scenario == "recovers" {
				f.mu.Lock()
				f.inspectError = ""
				f.state.Ready = true
				f.mu.Unlock()
				s.prepare(ctx, a)
				if !VMKernelRuntimeReady(a) {
					t.Fatal("same binding did not recover")
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.state.Generation != generation {
					t.Fatal("successful observation changed generation")
				}
			} else {
				s.prepare(ctx, a)
				f.mu.Lock()
				if f.state.Generation != generation {
					t.Fatal("recovered before retry bound")
				}
				f.mu.Unlock()
				s.prepare(ctx, a)
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.state.Generation <= generation {
					t.Fatal("persistent failure never recovered")
				}
			}
		})
	}
}

func TestVMKernelCredentialRevisionIgnoresBusinessMetadata(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	initial := vmCredentialRevision(a)
	a.Credentials["model_mapping"] = map[string]any{"gpt-6-luna": "gpt-6-luna"}
	a.Credentials["header_overrides"] = map[string]any{"x-example": "configured"}
	a.Credentials["refresh_token"] = "host-only-refresh"
	if initial != vmCredentialRevision(a) || !VMKernelRuntimeReady(a) {
		t.Fatal("business metadata rotated credential projection")
	}
	s.prepare(context.Background(), a)
	if f.state.Generation != 1 || f.kernelCalls != 0 {
		t.Fatal("business metadata restarted guest")
	}
	a.Credentials["access_token"] = "new-token"
	if initial == vmCredentialRevision(a) || VMKernelRuntimeReady(a) {
		t.Fatal("actual credential rotation was not fenced")
	}
}

func TestVMKernelProbeModelUsesAccountAllowlist(t *testing.T) {
	_, a, _ := vmLifecycleSetup(t)
	a.Credentials["model_mapping"] = map[string]any{"gpt-6-luna": "gpt-6-luna"}
	if model, err := vmKernelProbeModel(a, ""); err != nil || model != "gpt-6-luna" {
		t.Fatalf("model=%s err=%v", model, err)
	}
	if _, err := vmKernelProbeModel(a, "not-allowed"); err == nil {
		t.Fatal("explicit disallowed model accepted")
	}
}

func TestVMKernelCancellationDrainsConcurrentPeerBeforeRecovery(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	f.kernelEntered, f.kernelRelease = make(chan struct{}, 2), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled, peer := make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.Execute(ctx, a, "responses", []byte(`{}`), nil); cancelled <- err }()
	select {
	case <-f.kernelEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("first request not entered")
	}
	go func() { _, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil); peer <- err }()
	select {
	case <-f.kernelEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("peer not concurrent")
	}
	cancel()
	select {
	case err := <-cancelled:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not finish")
	}
	if VMKernelRuntimeReady(a) {
		t.Fatal("unknown cancellation must stop new admission")
	}
	s.prepare(context.Background(), a)
	if f.state.Generation != 1 {
		t.Fatal("recovery replaced an active peer's guest")
	}
	select {
	case err := <-peer:
		t.Fatalf("peer interrupted by cancellation: %v", err)
	default:
	}
	close(f.kernelRelease)
	if err := <-peer; err != nil {
		t.Fatalf("peer failed: %v", err)
	}
	s.prepare(context.Background(), a)
	if f.state.Generation != 2 || !VMKernelRuntimeReady(a) {
		t.Fatal("drained runtime was not recovered")
	}
}

func TestVMKernelDeletedAccountMarkDoesNotWaitForHelperOrBootstrap(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	vmKernelServices.Store(a.Platform, s)
	slot := s.slot(a.ID)
	slot.mu.Lock()
	done := make(chan struct{})
	go func() { MarkVMKernelAccountDeleting(a); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		slot.mu.Unlock()
		t.Fatal("account deletion blocked behind bootstrap")
	}
	slot.mu.Unlock()
	if VMKernelRuntimeReady(a) || !slot.revoked.Load() {
		t.Fatal("deleted account remained ready")
	}
	s.publish(a, f.state, nil)
	if VMKernelRuntimeReady(a) {
		t.Fatal("late observation revived deleted account")
	}
}

func TestVMKernelKnownUpstreamFailureDoesNotRebuildGuest(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	f.kernelKnownFailure = true
	result, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil)
	if err == nil || result == nil || !result.Settled || result.Completed {
		t.Fatalf("unexpected rejected outcome: %+v %v", result, err)
	}
	if s.slot(a.ID).quarantined || !VMKernelRuntimeReady(a) {
		t.Fatal("known upstream rejection quarantined healthy guest")
	}
	f.kernelKnownFailure = false
	s.prepare(context.Background(), a)
	if f.state.Generation != 1 {
		t.Fatal("known upstream rejection restarted guest")
	}
}

func waitVMKernelCapacityWaiter(t *testing.T, s *VMKernelService, id int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		slot := s.slot(id)
		slot.mu.Lock()
		waiting := slot.activeChanged != nil
		slot.mu.Unlock()
		if waiting {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("request did not wait for account capacity")
}

func TestVMKernelConfiguredConcurrencySharesProbeCapacityAndWakesOnAnyRelease(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	a.Concurrency = 2
	s.prepare(context.Background(), a)
	f.kernelEntered, f.kernelRelease = make(chan struct{}, 3), make(chan struct{}, 3)
	release := f.kernelRelease
	t.Cleanup(func() { close(release) })
	finished := make(chan error, 3)
	for i := 0; i < 2; i++ {
		go func() { _, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil); finished <- err }()
		select {
		case <-f.kernelEntered:
		case <-time.After(3 * time.Second):
			t.Fatal("configured concurrent request did not enter")
		}
	}
	go func() {
		_, err := s.ExecuteProbe(context.Background(), a, "responses", []byte(`{}`), nil)
		finished <- err
	}()
	waitVMKernelCapacityWaiter(t, s, a.ID)
	select {
	case <-f.kernelEntered:
		t.Fatal("probe bypassed configured account concurrency")
	default:
	}
	f.kernelRelease <- struct{}{}
	select {
	case <-f.kernelEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("probe waited for all peers instead of one free slot")
	}
	slot := s.slot(a.ID)
	slot.mu.Lock()
	active := slot.active
	slot.mu.Unlock()
	if active != 2 {
		t.Fatalf("active=%d, want two shared business/probe attempts", active)
	}
	f.kernelRelease <- struct{}{}
	f.kernelRelease <- struct{}{}
	for i := 0; i < 3; i++ {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
}

func TestVMKernelCapacityWaitCancellationDoesNotDispatchOrQuarantine(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	a.Concurrency = 1
	s.prepare(context.Background(), a)
	f.kernelEntered, f.kernelRelease = make(chan struct{}, 2), make(chan struct{}, 1)
	release := f.kernelRelease
	t.Cleanup(func() { close(release) })
	peer := make(chan error, 1)
	go func() { _, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil); peer <- err }()
	select {
	case <-f.kernelEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("peer did not enter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waited := make(chan error, 1)
	go func() { _, err := s.ExecuteProbe(ctx, a, "responses", []byte(`{}`), nil); waited <- err }()
	waitVMKernelCapacityWaiter(t, s, a.ID)
	cancel()
	select {
	case err := <-waited:
		if err != context.Canceled {
			t.Fatalf("waiting cancellation=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capacity wait ignored cancellation")
	}
	select {
	case <-f.kernelEntered:
		t.Fatal("cancelled waiter reached guest")
	default:
	}
	if !VMKernelRuntimeReady(a) || s.slot(a.ID).quarantined {
		t.Fatal("undispatched cancellation quarantined peer")
	}
	f.kernelRelease <- struct{}{}
	if err := <-peer; err != nil {
		t.Fatal(err)
	}
}

func TestVMKernelTwoActiveAttemptsDrainBeforeCredentialOrProxyRotation(t *testing.T) {
	for _, change := range []string{"credential", "proxy"} {
		t.Run(change, func(t *testing.T) {
			s, a, f := vmLifecycleSetup(t)
			a.Concurrency = 2
			s.prepare(context.Background(), a)
			f.kernelEntered, f.kernelRelease = make(chan struct{}, 2), make(chan struct{}, 2)
			release := f.kernelRelease
			t.Cleanup(func() { close(release) })
			finished := make(chan error, 2)
			for i := 0; i < 2; i++ {
				go func() { _, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil); finished <- err }()
				select {
				case <-f.kernelEntered:
				case <-time.After(3 * time.Second):
					t.Fatal("concurrent request did not enter")
				}
			}
			updated := *a
			if change == "credential" {
				updated.Credentials = map[string]any{"access_token": "rotated"}
			} else {
				proxy := *a.Proxy
				proxy.Host = "rotated.invalid"
				updated.Proxy = &proxy
			}
			s.repo.(*vmLifecycleRepo).account = &updated
			s.prepare(context.Background(), &updated)
			if f.state.Generation != 1 || VMKernelRuntimeReady(&updated) {
				t.Fatal("configuration applied before active requests drained")
			}
			f.kernelRelease <- struct{}{}
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			s.prepare(context.Background(), &updated)
			if f.state.Generation != 1 {
				t.Fatal("configuration interrupted remaining peer")
			}
			f.kernelRelease <- struct{}{}
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.kernelRelease = nil
			f.mu.Unlock()
			s.prepare(context.Background(), &updated)
			if f.state.Generation != 2 || !VMKernelRuntimeReady(&updated) {
				t.Fatal("configuration did not advance exactly one generation after drain")
			}
		})
	}
}

func TestVMKernelCapacityWaitRechecksLatestAccount(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	// The selected snapshot is stale. Admission must use the repository limit.
	a.Concurrency = 8
	latest := *a
	latest.Concurrency = 1
	s.repo.(*vmLifecycleRepo).account = &latest
	s.prepare(context.Background(), &latest)
	f.kernelEntered, f.kernelRelease = make(chan struct{}, 2), make(chan struct{}, 1)
	release := f.kernelRelease
	t.Cleanup(func() { close(release) })
	peer, waited := make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil); peer <- err }()
	select {
	case <-f.kernelEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("peer did not enter")
	}
	go func() { _, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil); waited <- err }()
	waitVMKernelCapacityWaiter(t, s, a.ID)
	// Publish an immutable replacement while the waiting request has released
	// its admission lock. The peer completion wakes admission through that lock.
	updated := latest
	updated.Status = StatusDisabled
	s.repo.(*vmLifecycleRepo).account = &updated
	f.kernelRelease <- struct{}{}
	if err := <-peer; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("waiting request ignored newly disabled account")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiting request did not revalidate after release")
	}
	select {
	case <-f.kernelEntered:
		t.Fatal("waiting request dispatched stale account configuration")
	default:
	}
}

func TestVMKernelThirtyConcurrentAdmissionsReuseControlPlaneObservation(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	a.Concurrency = 30
	s.prepare(context.Background(), a)
	observations := len(f.operations())
	f.kernelEntered, f.kernelRelease = make(chan struct{}, 30), make(chan struct{})
	release := f.kernelRelease
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finish)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 30)
	for i := 0; i < 30; i++ {
		go func() { _, err := s.Execute(ctx, a, "responses", []byte(`{}`), nil); finished <- err }()
	}
	for i := 0; i < 30; i++ {
		select {
		case <-f.kernelEntered:
		case <-ctx.Done():
			t.Fatalf("only %d of 30 requests reached guest concurrently", i)
		}
	}
	if len(f.operations()) != observations {
		t.Fatal("public admission repeated slow helper observations")
	}
	// Release only after all 30 attempts are simultaneously inside the guest.
	finish()
	for i := 0; i < 30; i++ {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
}

func TestVMKernelLocalReadinessDoesNotRequireSuccessfulModelGeneration(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	f.kernelFail = true
	s.prepare(context.Background(), a)
	if !VMKernelRuntimeReady(a) || f.kernelCalls != 0 {
		t.Fatal("local readiness attempted a provider model generation")
	}
	for i := 0; i < 3; i++ {
		s.prepare(context.Background(), a)
	}
	if f.state.Generation != 1 || f.kernelCalls != 0 {
		t.Fatal("provider state caused repeated generation or model probes")
	}
}

func TestVMKernelAdmissionRejectsExpiredOrChangedControlPlaneBinding(t *testing.T) {
	for _, scenario := range []string{"expired", "generation"} {
		t.Run(scenario, func(t *testing.T) {
			s, a, f := vmLifecycleSetup(t)
			s.prepare(context.Background(), a)
			value, _ := vmKernelReadiness.Load(a.ID)
			status := value.(vmRuntimeStatus)
			if scenario == "expired" {
				status.checked = time.Now().Add(-time.Minute)
			} else {
				status.state.Generation++
			}
			vmKernelReadiness.Store(a.ID, status)
			if _, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil); err == nil || f.kernelCalls != 0 {
				t.Fatalf("stale control-plane observation admitted: %v", err)
			}
		})
	}
}

func TestVMKernelHTTPProviderErrorKeepsReadyAndDoesNotRebuild(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(context.Background(), a)
	f.mu.Lock()
	f.kernelHTTPProviderFailure = true
	f.mu.Unlock()
	result, err := s.Execute(context.Background(), a, "responses", []byte(`{}`), nil)
	if err == nil || result == nil || !result.Dispatched || !result.Settled || result.Completed || len(result.ErrorEvent) == 0 {
		t.Fatalf("HTTP provider error was not classified as settled: result=%+v err=%v", result, err)
	}
	if !VMKernelRuntimeReady(a) || s.slot(a.ID).quarantined {
		t.Fatal("HTTP provider rejection isolated a healthy VM")
	}
	before := len(f.operations())
	s.prepare(context.Background(), a)
	operations := f.operations()
	if f.state.Generation != 1 || len(operations) != before+1 || operations[before] != "inspect:0" || !VMKernelRuntimeReady(a) {
		t.Fatalf("provider rejection caused VM recovery: generation=%d operations=%v", f.state.Generation, operations)
	}
	f.mu.Lock()
	f.kernelHTTPProviderFailure = false
	f.mu.Unlock()
	result, err = s.Execute(context.Background(), a, "responses", []byte(`{}`), nil)
	if err != nil || result == nil || !result.Completed || !VMKernelRuntimeReady(a) {
		t.Fatalf("next request could not use the same healthy binding: result=%+v err=%v", result, err)
	}
	if f.state.Generation != 1 {
		t.Fatal("next dispatch unexpectedly changed VM generation")
	}
}

type vmFailingTokens struct{}

func (vmFailingTokens) GetAccessToken(context.Context, *Account) (string, error) {
	return "", fmt.Errorf("oauth_refresh_failed")
}

func TestVMKernelBusinessIneligibilityDoesNotStopHealthyContainer(t *testing.T) {
	for _, scenario := range []string{"proxy_disabled", "proxy_expired", "account_disabled", "oauth_refresh"} {
		t.Run(scenario, func(t *testing.T) {
			s, a, f := vmLifecycleSetup(t)
			s.prepare(context.Background(), a)
			f.requests = nil
			switch scenario {
			case "proxy_disabled":
				a.Proxy.Status = StatusDisabled
			case "proxy_expired":
				expired := time.Now().Add(-time.Hour)
				a.Proxy.ExpiresAt = &expired
			case "account_disabled":
				a.Status = StatusDisabled
			case "oauth_refresh":
				s.tokens = vmFailingTokens{}
			}
			s.prepare(context.Background(), a)
			if fmt.Sprint(f.operations()) != "[inspect:0]" {
				t.Fatalf("business failure mutated container: %v", f.operations())
			}
			if VMKernelRuntimeReady(a) {
				t.Fatal("business-ineligible account admitted requests")
			}
			if summary := VMKernelRuntimeSummary(a); summary == nil || !summary.Ready || summary.ErrorCode != "" {
				t.Fatalf("business failure corrupted local health: %+v", summary)
			}
		})
	}
}
