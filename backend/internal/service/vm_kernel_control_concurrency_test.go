package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
)

// These barriers pause control-plane work without holding the fixture mutex:
// business handlers must be able to enter the kernel while the barrier is shut.
type vmControlBarrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newVMControlBarrier(t *testing.T) *vmControlBarrier {
	t.Helper()
	b := &vmControlBarrier{entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(b.unblock)
	return b
}

func (b *vmControlBarrier) unblock() { b.once.Do(func() { close(b.release) }) }

func (b *vmControlBarrier) wait(ctx context.Context) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func vmControlWait(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func vmControlAwait(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal(message)
		case <-tick.C:
		}
	}
}

func vmControlStartPrepare(s *VMKernelService, ctx context.Context, a *Account) <-chan struct{} {
	done := make(chan struct{})
	go func() { s.prepare(ctx, a); close(done) }()
	return done
}

func vmControlBlockInspection(t *testing.T, s *VMKernelService) *vmControlBarrier {
	t.Helper()
	b := newVMControlBarrier(t)
	original := s.helper
	var blocked atomic.Bool
	socket := vmForwardUDS(t, func(w http.ResponseWriter, r *http.Request) {
		var request vmruntime.Request
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Operation == "inspect" && blocked.CompareAndSwap(false, true) {
			if b.wait(r.Context()) != nil {
				return
			}
		}
		state, err := original.Do(r.Context(), request)
		if err != nil {
			var fault *vmruntime.Error
			if !errors.As(err, &fault) {
				fault = &vmruntime.Error{Code: "fixture_inspect_failed"}
			}
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"state": state, "error": fault})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": state})
	})
	s.helper = vmruntime.NewClient(socket)
	t.Cleanup(s.helper.CloseIdleConnections)
	return b
}

type vmControlTokens struct {
	barrier *vmControlBarrier
	calls   atomic.Int32
}

func (p *vmControlTokens) GetAccessToken(ctx context.Context, _ *Account) (string, error) {
	p.calls.Add(1)
	if p.barrier != nil {
		if err := p.barrier.wait(ctx); err != nil {
			return "", err
		}
	}
	return "token", nil
}

type vmControlSnapshotRepo struct {
	AccountRepository
	account atomic.Pointer[Account]
}

func (r *vmControlSnapshotRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account.Load(), nil
}

func vmControlUseSnapshotRepo(s *VMKernelService, a *Account) *vmControlSnapshotRepo {
	r := &vmControlSnapshotRepo{}
	r.account.Store(a)
	s.repo = r
	return r
}

func TestVMKernelControlWaitAllowsHealthyConcurrentRequests(t *testing.T) {
	for _, stage := range []string{"inspect", "token"} {
		t.Run(stage, func(t *testing.T) {
			s, a, f := vmLifecycleSetup(t)
			a.Concurrency = 2
			s.prepare(t.Context(), a)
			before := len(f.operations())
			var barrier *vmControlBarrier
			if stage == "inspect" {
				barrier = vmControlBlockInspection(t, s)
			} else {
				barrier = newVMControlBarrier(t)
				s.tokens = &vmControlTokens{barrier: barrier}
			}
			prepared := vmControlStartPrepare(s, t.Context(), a)
			vmControlWait(t, barrier.entered, "control operation did not reach barrier")
			kernel := newVMControlBarrier(t)
			f.mu.Lock()
			f.kernelEntered, f.kernelRelease = make(chan struct{}, 2), kernel.release
			entered := f.kernelEntered
			f.mu.Unlock()
			finished := make(chan error, 2)
			for i := 0; i < 2; i++ {
				go func() { _, err := s.Execute(t.Context(), a, "responses", []byte(`{}`), nil); finished <- err }()
			}
			for i := 0; i < 2; i++ {
				vmControlWait(t, entered, "healthy request waited for slow control operation")
			}
			kernel.unblock()
			for i := 0; i < 2; i++ {
				select {
				case err := <-finished:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("attempt completion waited for slow control operation")
				}
			}
			select {
			case <-prepared:
				t.Fatal("control barrier unexpectedly released")
			default:
			}
			barrier.unblock()
			vmControlWait(t, prepared, "control operation did not finish")
			if !VMKernelRuntimeReady(a) || f.state.Generation != 1 || len(f.operations()) != before+1 {
				t.Fatal("healthy inspection mutated binding or lost readiness")
			}
		})
	}
}

func TestVMKernelControlLateInspectionCannotReviveRevokedAccount(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(t.Context(), a)
	vmKernelServices.Store(a.Platform, s)
	oldHealth, _ := vmKernelHealth.Load(a.ID)
	barrier := vmControlBlockInspection(t, s)
	prepared := vmControlStartPrepare(s, t.Context(), a)
	vmControlWait(t, barrier.entered, "inspection did not start")
	MarkVMKernelAccountDeleting(a)
	barrier.unblock()
	vmControlWait(t, prepared, "late inspection did not finish")
	health, _ := vmKernelHealth.Load(a.ID)
	if VMKernelRuntimeReady(a) || health.(vmRuntimeStatus).checked != oldHealth.(vmRuntimeStatus).checked || f.state.Generation != 1 {
		t.Fatal("late observation republished state after revocation")
	}
	if _, exists := vmKernelReadiness.Load(a.ID); exists {
		t.Fatal("revoked account regained a readiness receipt")
	}
}

func TestVMKernelControlDeletionFencesBeforeWaitingForInspection(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(t.Context(), a)
	vmKernelServices.Store(a.Platform, s)
	barrier := vmControlBlockInspection(t, s)
	prepared := vmControlStartPrepare(s, t.Context(), a)
	vmControlWait(t, barrier.entered, "inspection did not start")
	kernel := newVMControlBarrier(t)
	f.mu.Lock()
	f.kernelEntered, f.kernelRelease = make(chan struct{}, 1), kernel.release
	entered := f.kernelEntered
	f.mu.Unlock()
	finished := make(chan error, 1)
	go func() { _, err := s.Execute(t.Context(), a, "responses", []byte(`{}`), nil); finished <- err }()
	vmControlWait(t, entered, "active request did not enter kernel")
	deleted := make(chan error, 1)
	go func() { deleted <- PrepareVMKernelAccountDeletion(t.Context(), a) }()
	slot := s.slot(a.ID)
	vmControlAwait(t, func() bool {
		if !slot.mu.TryLock() {
			return false
		}
		defer slot.mu.Unlock()
		return slot.deleting
	}, "deletion could not fence an in-flight observation")
	if _, err := s.Execute(t.Context(), a, "responses", []byte(`{}`), nil); err == nil {
		t.Fatal("new request passed deletion fence")
	}
	kernel.unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deletion held admission while waiting for inspection")
	}
	select {
	case err := <-deleted:
		t.Fatalf("deletion overtook the control operation: %v", err)
	default:
	}
	barrier.unblock()
	vmControlWait(t, prepared, "late inspection did not finish")
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deletion deadlocked behind observation")
	}
	if VMKernelRuntimeReady(a) || f.state.Phase != "removed" || f.state.Generation != 2 {
		t.Fatal("deletion did not remove exactly the observed generation")
	}
	for _, operation := range f.operations() {
		if operation == "prepare:2" || operation == "apply-config:2" {
			t.Fatal("late observation revived a deleting account")
		}
	}
}

func TestVMKernelControlRereadsBusinessStateAfterSlowOperations(t *testing.T) {
	for _, stage := range []string{"inspect", "token"} {
		for _, pause := range []string{StatusDisabled, OpenAIContainerAuthRequired, OpenAIContainerTwoFAEnrollmentPending} {
			t.Run(stage+"/"+pause, func(t *testing.T) {
				s, a, f := vmLifecycleSetup(t)
				s.prepare(t.Context(), a)
				repo := vmControlUseSnapshotRepo(s, a)
				tokens := &vmControlTokens{}
				s.tokens = tokens
				var barrier *vmControlBarrier
				if stage == "inspect" {
					barrier = vmControlBlockInspection(t, s)
				} else {
					barrier = newVMControlBarrier(t)
					tokens.barrier = barrier
				}
				prepared := vmControlStartPrepare(s, t.Context(), a)
				vmControlWait(t, barrier.entered, "control operation did not start")
				latest := *a
				if pause == StatusDisabled {
					latest.Status = pause
				} else {
					latest.Status, latest.ErrorMessage = StatusError, pause
				}
				repo.account.Store(&latest)
				barrier.unblock()
				vmControlWait(t, prepared, "control operation did not finish")
				if VMKernelRuntimeReady(&latest) || f.state.Generation != 1 {
					t.Fatal("stale business state was admitted or mutated")
				}
				if stage == "inspect" && tokens.calls.Load() != 0 {
					t.Fatal("stale observation refreshed a paused account")
				}
				if summary := VMKernelRuntimeSummary(&latest); summary == nil || !summary.Ready {
					t.Fatal("business pause erased independent container health")
				}
			})
		}
	}
}

func TestVMKernelControlFailedObservationFencesBeforeTokenWait(t *testing.T) {
	for _, observation := range []string{"failed", "generation", "not_ready"} {
		t.Run(observation, func(t *testing.T) {
			s, a, f := vmLifecycleSetup(t)
			s.prepare(t.Context(), a)
			f.mu.Lock()
			switch observation {
			case "failed":
				f.inspectError = "docker_operation_unknown"
			case "generation":
				f.state.Generation++
			case "not_ready":
				f.state.Ready = false
			}
			f.mu.Unlock()
			barrier := newVMControlBarrier(t)
			s.tokens = &vmControlTokens{barrier: barrier}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			prepared := vmControlStartPrepare(s, ctx, a)
			vmControlWait(t, barrier.entered, "token operation did not start")
			if VMKernelRuntimeReady(a) {
				t.Fatal("bad observation left old readiness valid during refresh")
			}
			if _, err := s.Execute(t.Context(), a, "responses", []byte(`{}`), nil); err == nil {
				t.Fatal("bad observation admitted a request")
			}
			cancel()
			vmControlWait(t, prepared, "cancelled token wait did not exit")
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.kernelCalls != 0 {
				t.Fatal("fenced request reached kernel")
			}
		})
	}
}

func TestVMKernelControlRotationAfterTokenWaitStillDrainsAttempts(t *testing.T) {
	for _, rotation := range []string{"credential", "proxy"} {
		t.Run(rotation, func(t *testing.T) {
			s, a, f := vmLifecycleSetup(t)
			a.Concurrency = 2
			s.prepare(t.Context(), a)
			repo := vmControlUseSnapshotRepo(s, a)
			barrier := newVMControlBarrier(t)
			s.tokens = &vmControlTokens{barrier: barrier}
			prepared := vmControlStartPrepare(s, t.Context(), a)
			vmControlWait(t, barrier.entered, "token operation did not start")
			f.mu.Lock()
			f.kernelEntered, f.kernelRelease = make(chan struct{}, 2), make(chan struct{}, 2)
			entered, release := f.kernelEntered, f.kernelRelease
			f.mu.Unlock()
			t.Cleanup(func() { close(release) })
			finished := make(chan error, 2)
			for i := 0; i < 2; i++ {
				go func() { _, err := s.Execute(t.Context(), a, "responses", []byte(`{}`), nil); finished <- err }()
			}
			for i := 0; i < 2; i++ {
				vmControlWait(t, entered, "peer did not enter kernel during refresh")
			}
			latest := *a
			if rotation == "credential" {
				latest.Credentials = map[string]any{"access_token": "rotated"}
			} else {
				proxy := *a.Proxy
				proxy.Host = "rotated.invalid"
				latest.Proxy = &proxy
			}
			repo.account.Store(&latest)
			barrier.unblock()
			vmControlWait(t, prepared, "rotation observation did not finish")
			if f.state.Generation != 1 || VMKernelRuntimeReady(&latest) {
				t.Fatal("rotation applied before active attempts drained")
			}
			if _, err := s.Execute(t.Context(), a, "responses", []byte(`{}`), nil); err == nil {
				t.Fatal("stale selected account survived rotation")
			}
			s.tokens = vmLifecycleTokens{}
			for i := 0; i < 2; i++ {
				release <- struct{}{}
				select {
				case err := <-finished:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("active attempt failed to drain")
				}
				s.prepare(t.Context(), &latest)
				if i == 0 && f.state.Generation != 1 {
					t.Fatal("rotation interrupted the remaining attempt")
				}
			}
			if f.state.Generation != 2 || !VMKernelRuntimeReady(&latest) {
				t.Fatal("drained rotation did not advance exactly one generation")
			}
		})
	}
}

func TestVMKernelControlHealthLaneWaitHonorsCancellation(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(t.Context(), a)
	before := len(f.operations())
	s.controlLimits()
	for i := 0; i < cap(s.healthLimit); i++ {
		s.healthLimit <- struct{}{}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	prepared := vmControlStartPrepare(s, ctx, a)
	slot := s.slot(a.ID)
	vmControlAwait(t, func() bool {
		if slot.controlMu.TryLock() {
			slot.controlMu.Unlock()
			return false
		}
		return true
	}, "control operation did not acquire its lane")
	vmControlAwait(t, func() bool {
		if !slot.mu.TryLock() {
			return false
		}
		slot.mu.Unlock()
		return true
	}, "health lane wait held admission lock")
	cancel()
	vmControlWait(t, prepared, "health lane wait ignored cancellation")
	if len(f.operations()) != before || !slot.controlMu.TryLock() {
		t.Fatal("cancelled wait leaked control lock or called helper")
	}
	slot.controlMu.Unlock()
	for i := 0; i < cap(s.healthLimit); i++ {
		<-s.healthLimit
	}
}

func TestVMKernelControlDeletionWaitHonorsCancellation(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(t.Context(), a)
	vmKernelServices.Store(a.Platform, s)
	barrier := vmControlBlockInspection(t, s)
	prepared := vmControlStartPrepare(s, t.Context(), a)
	vmControlWait(t, barrier.entered, "inspection did not start")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	deleted := make(chan error, 1)
	go func() { deleted <- PrepareVMKernelAccountDeletion(ctx, a) }()
	slot := s.slot(a.ID)
	vmControlAwait(t, func() bool {
		if !slot.mu.TryLock() {
			return false
		}
		defer slot.mu.Unlock()
		return slot.deleting
	}, "deletion could not fence the account")
	cancel()
	select {
	case err := <-deleted:
		if err == nil {
			t.Fatal("cancelled deletion reported removal")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("control lock wait ignored deletion cancellation")
	}
	if !slot.mu.TryLock() {
		t.Fatal("cancelled deletion leaked admission lock")
	}
	deleting := slot.deleting
	slot.mu.Unlock()
	if !deleting || VMKernelRuntimeReady(a) {
		t.Fatal("cancelled deletion cleared its admission fence")
	}
	barrier.unblock()
	vmControlWait(t, prepared, "late inspection did not finish")
	if VMKernelRuntimeReady(a) || f.state.Generation != 1 {
		t.Fatal("late inspection mutated a fenced account")
	}
}

func TestVMKernelControlOperationsSerializeWithoutAdmissionLock(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(t.Context(), a)
	before := len(f.operations())
	barrier := vmControlBlockInspection(t, s)
	prepared := vmControlStartPrepare(s, t.Context(), a)
	vmControlWait(t, barrier.entered, "inspection did not start")
	s.prepare(t.Context(), a)
	s.retire(t.Context(), a.ID, true)
	if len(f.operations()) != before || !VMKernelRuntimeReady(a) {
		t.Fatal("overlapping lifecycle operation bypassed control serialization")
	}
	barrier.unblock()
	vmControlWait(t, prepared, "inspection did not finish")
	s.retire(t.Context(), a.ID, true)
	if f.state.Phase != "removed" || f.state.Generation != 2 || VMKernelRuntimeReady(a) {
		t.Fatal("retirement failed after the control lane was released")
	}
}
