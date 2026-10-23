package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
	"github.com/tidwall/gjson"
)

type vmRuntimeStatus struct {
	state              vmruntime.State
	credentials, proxy string
	checked            time.Time
}

// Local readiness supplements the original administrator, group and quota
// policy. It never writes the administrator-owned Schedulable flag.
var vmKernelReadiness sync.Map

// Independent local health observations; never derived from admission or provider errors.
var vmKernelHealth sync.Map
var vmKernelServices sync.Map

func VMKernelRuntimeReady(a *Account) bool {
	if a != nil && a.ExecutionMode == "vm_kernel" {
		return false
	}
	if a == nil || !a.IsVMKernel() {
		return true
	}
	if service := LookupVMKernelService(a.Platform); service != nil {
		if value, ok := service.slots.Load(a.ID); ok && value.(*vmKernelSlot).revoked.Load() {
			return false
		}
	}
	value, ok := vmKernelReadiness.Load(a.ID)
	if !ok {
		return false
	}
	status := value.(vmRuntimeStatus)
	credential, proxy := vmCredentialRevision(a), vmProxyRevision(a)
	if revision := a.VMKernelSchedulerRevision; revision != nil {
		// This marker is generated only by the trusted metadata projection. Full
		// accounts must retain their live fields and cannot override their fence.
		if a.GetCredential("access_token") != "" || a.Proxy != nil || revision.Credential == "" || revision.Proxy == "" {
			return false
		}
		credential, proxy = revision.Credential, revision.Proxy
	}
	return credential != "" && status.state.Ready && !status.state.DeleteRequested && time.Since(status.checked) < 30*time.Second && status.credentials == credential && status.proxy == proxy
}
func LookupVMKernelService(platform string) *VMKernelService {
	value, ok := vmKernelServices.Load(platform)
	if !ok {
		return nil
	}
	return value.(*VMKernelService)
}
func StopVMKernelServices() {
	vmKernelServices.Range(func(_, value any) bool { value.(*VMKernelService).Stop(); return true })
}

type vmKernelTokenProvider interface {
	GetAccessToken(context.Context, *Account) (string, error)
}

// Each account owns one lifecycle binding, with independent concurrent attempts.
// The existing scheduler owns business leases; this lane also bounds management
// and business calls together by the current account concurrency configuration.
// A control-plane observation can briefly hold the same account lock used for
// admission. Context-aware acquisition lets admission wait for that observation
// without queueing behind an active inference or an unbounded bootstrap.
type vmKernelAccountMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *vmKernelAccountMutex) init() {
	m.once.Do(func() { m.token = make(chan struct{}, 1); m.token <- struct{}{} })
}
func (m *vmKernelAccountMutex) Lock()   { m.init(); <-m.token }
func (m *vmKernelAccountMutex) Unlock() { m.token <- struct{}{} }
func (m *vmKernelAccountMutex) TryLock() bool {
	m.init()
	select {
	case <-m.token:
		return true
	default:
		return false
	}
}
func (m *vmKernelAccountMutex) lockContext(ctx context.Context) bool {
	m.init()
	select {
	case <-m.token:
		return true
	case <-ctx.Done():
		return false
	}
}

type vmKernelSlot struct {
	mu                  vmKernelAccountMutex
	active              int
	activeDone          chan struct{}
	activeChanged       chan struct{}
	deleting            bool
	revoked             atomic.Bool
	observationFailures int
	verifiedBinding     string
	nextProbe           time.Time
}
type VMKernelService struct {
	cfg                config.VMKernelConfig
	repo               AccountRepository
	platform           string
	tokens             vmKernelTokenProvider
	helper             *vmruntime.Client
	kernel             *vmkernel.Client
	slots              sync.Map
	cancel             context.CancelFunc
	done               chan struct{}
	stopOnce           sync.Once
	releaseCoordinator func()
	workerMu           sync.Mutex
	workers            sync.WaitGroup
	workerStopped      bool
	inflight           map[int64]bool
	limitsOnce         sync.Once
	bootstrapLimit     chan struct{}
	healthLimit        chan struct{}
}

// Use the ordinary gateway's timeout policy; Docker mode has no extra deadline.
func newVMKernelClient(cfg *config.Config, platform string) *vmkernel.Client {
	client := vmkernel.NewClient()
	seconds := cfg.Gateway.ResponseHeaderTimeout
	if platform == PlatformOpenAI {
		seconds = cfg.Gateway.OpenAIResponseHeaderTimeout
	}
	client.ResponseHeaderTimeout = time.Duration(seconds) * time.Second
	return client
}

func NewVMKernelService(cfg *config.Config, repo AccountRepository, platform string, cp *ClaudeTokenProvider, op *OpenAITokenProvider) *VMKernelService {
	if cfg == nil || !cfg.VMKernel.Enabled || repo == nil || !filepath.IsAbs(cfg.VMKernel.HelperSocket) {
		return nil
	}
	release, err := acquireVMKernelCoordinator(cfg.VMKernel.HelperSocket)
	if err != nil {
		slog.Error("vm_kernel_coordinator_unavailable", "code", err.Error())
		return nil
	}
	s := &VMKernelService{cfg: cfg.VMKernel, repo: repo, platform: platform, helper: vmruntime.NewClient(cfg.VMKernel.HelperSocket), kernel: newVMKernelClient(cfg, platform), done: make(chan struct{})}
	s.releaseCoordinator = release
	if platform == PlatformAnthropic && cp != nil {
		s.tokens = cp
	}
	if platform == PlatformOpenAI && op != nil {
		s.tokens = op
	}
	if s.cfg.Template == "" {
		s.cfg.Template = "default"
	}
	if s.cfg.CPU <= 0 {
		s.cfg.CPU = 2
	}
	if s.cfg.MemoryMiB <= 0 {
		s.cfg.MemoryMiB = 512
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	if previous, exists := vmKernelServices.LoadOrStore(platform, s); exists {
		cancel()
		s.helper.CloseIdleConnections()
		release()
		return previous.(*VMKernelService)
	}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			s.reconcile(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return s
}
func (s *VMKernelService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.workerMu.Lock()
		s.workerStopped = true
		s.workerMu.Unlock()
		if s.cancel != nil {
			s.cancel()
			<-s.done
		}
		s.workers.Wait()
		s.slots.Range(func(key, _ any) bool { vmKernelReadiness.Delete(key); vmKernelHealth.Delete(key); return true })
		vmKernelServices.CompareAndDelete(s.platform, s)
		if s.helper != nil {
			s.helper.CloseIdleConnections()
		}
		if s.releaseCoordinator != nil {
			s.releaseCoordinator()
		}
	})
}
func (s *VMKernelService) slot(id int64) *vmKernelSlot {
	value, _ := s.slots.LoadOrStore(id, &vmKernelSlot{})
	return value.(*vmKernelSlot)
}
func vmRuntimeErrorCode(err error) string {
	var fault *vmruntime.Error
	if errors.As(err, &fault) {
		return fault.Code
	}
	return "runtime_operation_failed"
}
func vmRuntimeMissing(err error) bool {
	var fault *vmruntime.Error
	return errors.As(err, &fault) && fault.Code == "instance_not_found"
}
func vmPlatform(platform string) string {
	if platform == PlatformOpenAI {
		return "codex"
	}
	return "claude"
}
func vmBindingKey(s *vmruntime.State) string {
	return fmt.Sprintf("%s:%d:%s:%s:%s", s.InstanceID, s.Generation, s.GuestBootID, s.AppliedCredentialRevision, s.AppliedProxyRevision)
}
func vmAccountEligible(a *Account) bool {
	if a == nil || !a.IsVMKernel() || ValidateVMKernelAccount(a) != nil || a.ProxyID == nil || a.Proxy == nil || a.Proxy.Status != StatusActive || a.Proxy.IsExpired(time.Now()) {
		return false
	}
	// A paused, cooling-down or admission-pending account still needs its
	// runtime for authorized management probes and credential maintenance.
	return a.IsActive()
}

func vmAccountRequestEligible(a *Account, probe bool) bool {
	if !vmAccountEligible(a) {
		return false
	}
	if probe {
		return true
	}
	copy := *a
	copy.ExecutionMode = AccountExecutionModeNormal
	return copy.IsSchedulable()
}
func (s *VMKernelService) publish(a *Account, state *vmruntime.State, err error, stages ...string) {
	if s.slot(a.ID).revoked.Load() {
		vmKernelReadiness.Delete(a.ID)
		return
	}
	status := vmRuntimeStatus{credentials: vmCredentialRevision(a), proxy: vmProxyRevision(a), checked: time.Now()}
	if state != nil {
		status.state = *state
	}
	if err != nil {
		stage := "control"
		if len(stages) > 0 {
			stage = stages[0]
		}
		logVMKernelControlFailure(a.ID, stage, status.state.Generation, err)
		status.state.Ready = false
		status.state.Phase = "error"
		status.state.ErrorCode = vmRuntimeErrorCode(err)
	}
	vmKernelReadiness.Store(a.ID, status)
}
func (s *VMKernelService) reconcile(ctx context.Context) {
	lifecycle := ctx
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	accounts, err := s.repo.ListByPlatform(ctx, s.platform)
	if err != nil {
		return
	}
	// Inventory survives process restarts and contains tombstones. It is never a
	// health observation or a substitute for authoritative account reads.
	inventory, err := s.helper.Inventory(ctx)
	if err != nil {
		return
	}
	for _, state := range inventory {
		if state.Spec.Platform != vmPlatform(s.platform) {
			continue
		}
		a, e := s.repo.GetByID(ctx, state.AccountID)
		// Removed VM mode is retained as data only. Never migrate or destroy
		// its resources when the Docker coordinator starts.
		if e == nil && a != nil && a.ExecutionMode == "vm_kernel" {
			continue
		}
		if errors.Is(e, ErrAccountNotFound) || (e == nil && (a == nil || !a.IsVMKernel())) {
			id := state.AccountID
			s.enqueue(lifecycle, id, func(task context.Context) { s.retire(task, id, true) })
			continue
		}
		if e != nil {
			vmKernelReadiness.Delete(state.AccountID)
			continue
		}
	}
	for i := range accounts {
		a := accounts[i]
		if !a.IsVMKernel() {
			continue
		}
		s.enqueue(lifecycle, a.ID, func(task context.Context) { s.prepare(task, &a) })
	}
}

// A single task owns each account across reconciliation ticks. Slow boots do
// not postpone the next inventory or the readiness observations of other VMs.
func (s *VMKernelService) enqueue(ctx context.Context, id int64, work func(context.Context)) bool {
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	if s.workerStopped || ctx.Err() != nil || s.inflight[id] {
		return false
	}
	if s.inflight == nil {
		s.inflight = make(map[int64]bool)
	}
	s.inflight[id] = true
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer func() { s.workerMu.Lock(); delete(s.inflight, id); s.workerMu.Unlock() }()
		task, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		work(task)
	}()
	return true
}

func (s *VMKernelService) controlLimits() {
	s.limitsOnce.Do(func() {
		s.bootstrapLimit = make(chan struct{}, 4)
		s.healthLimit = make(chan struct{}, 16)
	})
}

// Public inference never creates or starts a VM. Every restart/projection
// change advances the helper generation and invalidates old WS bindings.
func (s *VMKernelService) prepare(ctx context.Context, account *Account) {
	if account != nil && account.ExecutionMode == "vm_kernel" {
		return
	}
	slot := s.slot(account.ID)
	if !slot.mu.TryLock() {
		return
	}
	defer slot.mu.Unlock()
	s.controlLimits()
	select {
	case s.healthLimit <- struct{}{}:
	case <-ctx.Done():
		return
	}
	observing := true
	defer func() {
		if observing {
			<-s.healthLimit
		}
	}()
	if slot.deleting || slot.revoked.Load() {
		return
	}
	// Inspect even when OAuth refresh or proxy business eligibility is failing.
	// Such failures must not stop a healthy process or erase its health evidence.
	state, inspectErr := s.helper.Do(ctx, vmruntime.Request{Operation: "inspect", AccountID: account.ID})
	s.publishHealth(account.ID, state, inspectErr)
	if !vmAccountEligible(account) {
		vmKernelReadiness.Delete(account.ID)
		return
	}
	if s.tokens == nil {
		s.publish(account, nil, errors.New("vm_token_provider_unavailable"), "credentials")
		return
	}
	if _, err := s.tokens.GetAccessToken(ctx, account); err != nil {
		s.publish(account, nil, err, "credentials")
		return
	}
	a, err := s.repo.GetByID(ctx, account.ID)
	if err != nil || !vmAccountEligible(a) {
		vmKernelReadiness.Delete(account.ID)
		return
	}
	credential, err := vmCredentialProjection(a)
	if err != nil {
		s.publish(a, nil, err, "credentials")
		return
	}
	err = inspectErr
	// A failed observation with a persisted binding can be drained/recovered at
	// a newer generation. A transport/inventory failure with no binding cannot
	// be reinterpreted as permission to create another instance.
	if err != nil && state == nil && !vmRuntimeMissing(err) {
		s.publish(a, state, err, "inspect")
		return
	}
	if state != nil && (state.AccountID != a.ID || state.InstanceID == "" || state.Generation == 0 || state.Spec.Platform != vmPlatform(a.Platform)) {
		s.publish(a, nil, errors.New("vm_binding_mismatch"), "inspect")
		return
	}
	if state != nil && state.DeleteRequested {
		s.publish(a, state, &vmruntime.Error{Code: "instance_deleting"}, "inspect")
		return
	}
	// A failed read-only observation never admits work. Give an already proven
	// unchanged binding two further observations before destructive recovery.
	eligibleObservationRetry := state != nil && slot.verifiedBinding != "" &&
		!slot.deleting && !state.DeleteRequested &&
		slot.verifiedBinding == vmBindingKey(state) &&
		state.AppliedCredentialRevision == vmCredentialRevision(a) && state.AppliedProxyRevision == vmProxyRevision(a)
	if eligibleObservationRetry && vmTransientObservationError(err) {
		slot.observationFailures++
		if slot.observationFailures < 3 {
			s.publish(a, state, err, "inspect")
			return
		}
	} else {
		slot.observationFailures = 0
	}
	projection := &vmruntime.Projection{Revision: vmCredentialRevision(a), Credential: credential, ProxyURL: a.Proxy.URL()}
	spec := vmruntime.Spec{Platform: vmPlatform(a.Platform), Template: s.cfg.Template, CPU: s.cfg.CPU, MemoryMiB: s.cfg.MemoryMiB, ProxyID: *a.ProxyID, ProxyRevision: vmProxyRevision(a)}
	request := vmruntime.Request{Operation: "prepare", AccountID: a.ID, Generation: 1, Spec: &spec, Projection: projection}
	mutate := state == nil
	if state != nil {
		request.InstanceID = state.InstanceID
		request.Generation = state.Generation + 1
		spec = state.Spec
		spec.ProxyID = *a.ProxyID
		spec.ProxyRevision = vmProxyRevision(a)
		changed := state.AppliedCredentialRevision != projection.Revision || state.AppliedProxyRevision != spec.ProxyRevision
		mutate = changed || !state.Ready || (slot.verifiedBinding != "" && slot.verifiedBinding != vmBindingKey(state))
		if changed {
			request.Operation = "apply-config"
		}
	}
	if mutate {
		// Configuration changes and failed health observations may replace the
		// guest. Stop new admission, but never interrupt other active attempts.
		if slot.active > 0 {
			vmKernelReadiness.Delete(a.ID)
			return
		}
		if time.Now().Before(slot.nextProbe) {
			return
		}
		// Release the observation lane before acquiring a cold-start slot.
		// All four bootstraps may take minutes; healthy inspections must proceed.
		<-s.healthLimit
		observing = false
		select {
		case s.bootstrapLimit <- struct{}{}:
		case <-ctx.Done():
			return
		default:
			// No helper mutation has started. Retry at the next inventory tick
			// without consuming the bootstrap deadline or blocking deletion.
			return
		}
		defer func() { <-s.bootstrapLimit }()
		// Recheck authority immediately before applying the observed projection.
		latest, readErr := s.repo.GetByID(ctx, a.ID)
		if readErr != nil || !vmAccountEligible(latest) || vmCredentialRevision(a) != vmCredentialRevision(latest) || vmAccountRouteRevision(a) != vmAccountRouteRevision(latest) {
			vmKernelReadiness.Delete(a.ID)
			return
		}
		vmKernelReadiness.Delete(a.ID)
		state, err = s.helper.Do(ctx, request)
		s.publishHealth(a.ID, state, err)
		if err != nil {
			s.publish(a, state, err, request.Operation)
			slot.nextProbe = time.Now().Add(time.Minute)
			return
		}
		slot.verifiedBinding = ""
		slot.observationFailures = 0
	}
	if !s.bindingReady(a, state) {
		s.publish(a, state, errors.New("vm_binding_not_ready"), "binding")
		return
	}
	// Readiness attests local guest/kernel health and its applied receipt.
	// Provider model generation is an ordinary request, never a boot gate.
	slot.verifiedBinding = vmBindingKey(state)
	slot.nextProbe = time.Time{}
	latest, err := s.repo.GetByID(ctx, a.ID)
	if err != nil || !vmAccountEligible(latest) || vmCredentialRevision(a) != vmCredentialRevision(latest) || vmAccountRouteRevision(a) != vmAccountRouteRevision(latest) {
		vmKernelReadiness.Delete(a.ID)
		return
	}
	s.publish(latest, state, nil)
}
func (s *VMKernelService) bindingReady(a *Account, state *vmruntime.State) bool {
	return state != nil && state.AccountID == a.ID && state.Spec.Platform == vmPlatform(a.Platform) && state.Ready && !state.DeleteRequested && state.Capacity >= 0 && state.Generation > 0 && state.InstanceID != "" && state.GuestBootID != "" && filepath.IsAbs(state.KernelSocket) && state.InternalToken != "" && state.AppliedCredentialRevision == vmCredentialRevision(a) && state.AppliedProxyRevision == vmProxyRevision(a)
}

// vmKernelProbeModel returns a supported ingress model; mapping remains the
// caller's responsibility, exactly as it is for ordinary account tests.
func vmKernelProbeModel(a *Account, requested string) (string, error) {
	if a == nil {
		return "", errors.New("vm_probe_account_missing")
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if a.IsModelSupported(requested) {
			return requested, nil
		}
		return "", errors.New("vm_probe_model_not_allowed")
	}
	model := openai.DefaultTestModel
	if a.Platform == PlatformAnthropic {
		model = claude.DefaultTestModel
	}
	if a.IsModelSupported(model) {
		return model, nil
	}
	var candidates []string
	for from, to := range a.GetModelMapping() {
		candidates = append(candidates, from, to)
	}
	slices.Sort(candidates)
	for _, candidate := range slices.Compact(candidates) {
		if candidate != "" && !strings.ContainsAny(candidate, "*?[]") && a.IsModelSupported(candidate) {
			return candidate, nil
		}
	}
	return "", errors.New("vm_probe_model_not_allowed")
}

func (s *VMKernelService) retire(ctx context.Context, id int64, remove bool) {
	vmKernelReadiness.Delete(id)
	slot := s.slot(id)
	if !slot.mu.TryLock() {
		return
	}
	defer slot.mu.Unlock()
	if slot.active > 0 {
		return
	}
	state, _ := s.helper.Do(ctx, vmruntime.Request{Operation: "inspect", AccountID: id})
	if state == nil || state.AccountID != id || state.InstanceID == "" || state.Generation == 0 {
		return
	}
	slot.verifiedBinding = ""
	operation := "stop"
	if remove || slot.deleting || slot.revoked.Load() || state.DeleteRequested {
		operation = "remove"
		slot.deleting = true
	}
	if state.Phase == "removed" || (operation == "stop" && state.Phase == "stopped") {
		vmKernelReadiness.Store(id, vmRuntimeStatus{state: *state, checked: time.Now()})
		return
	}
	state, err := s.helper.Do(ctx, vmruntime.Request{Operation: operation, AccountID: id, InstanceID: state.InstanceID, Generation: state.Generation + 1})
	s.publishHealth(id, state, err)
	if state != nil {
		state.Ready = false
		if err != nil {
			state.ErrorCode = vmRuntimeErrorCode(err)
		}
		vmKernelReadiness.Store(id, vmRuntimeStatus{state: *state, checked: time.Now()})
	}
}

// MarkVMKernelAccountDeleting is called only after the account repository has
// committed deletion. Inventory reconciliation owns durable resource cleanup;
// an unavailable helper must not roll back account revocation.
func MarkVMKernelAccountDeleting(a *Account) {
	if a == nil || !a.IsVMKernel() {
		return
	}
	vmKernelReadiness.Delete(a.ID)
	if s := LookupVMKernelService(a.Platform); s != nil {
		// Independent of the bootstrap mutex so deletion cannot block behind
		// cold start, and a completing observation cannot republish readiness.
		s.slot(a.ID).revoked.Store(true)
	}
}

// Keep DB authority until the helper persists deletion intent and confirms a
// stopped runner. Only the runner is removed; guest disks remain recoverable.
func PrepareVMKernelAccountDeletion(ctx context.Context, a *Account) error {
	if a == nil || !a.IsVMKernel() {
		return nil
	}
	s := LookupVMKernelService(a.Platform)
	if s == nil {
		return errors.New("vm_kernel_runtime_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	slot := s.slot(a.ID)
	slot.mu.Lock()
	slot.deleting = true
	vmKernelReadiness.Delete(a.ID)
	done := slot.activeDone
	slot.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.New("vm_kernel_delete_draining")
		}
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	state, err := s.helper.Do(ctx, vmruntime.Request{Operation: "inspect", AccountID: a.ID})
	if vmRuntimeMissing(err) {
		return nil
	}
	if err != nil && state == nil {
		return err
	}
	if state == nil || state.AccountID != a.ID || state.InstanceID == "" || state.Generation == 0 {
		return errors.New("vm_binding_not_found")
	}
	state, err = s.helper.Do(ctx, vmruntime.Request{Operation: "remove", AccountID: a.ID, InstanceID: state.InstanceID, Generation: state.Generation + 1})
	if err != nil {
		return err
	}
	if state == nil || !state.DeleteRequested || state.Phase != "removed" || state.ContainerID != "" {
		return errors.New("vm_removal_unconfirmed")
	}
	return nil
}
func (s *VMKernelService) Execute(ctx context.Context, selected *Account, protocol string, body []byte, emit func(vmkernel.Event) error) (*vmkernel.Result, error) {
	return s.execute(ctx, selected, protocol, body, emit, false)
}

// A management test can reach a healthy guest before business readiness but
// may bypass business scheduling pauses, but not disabled accounts or a changed binding.
func (s *VMKernelService) ExecuteProbe(ctx context.Context, selected *Account, protocol string, body []byte, emit func(vmkernel.Event) error) (*vmkernel.Result, error) {
	prepared, err := prepareVMKernelProbeBody(selected, protocol, body)
	if err != nil {
		return nil, err
	}
	return s.execute(ctx, selected, protocol, prepared, emit, true)
}

func prepareVMKernelProbeBody(account *Account, protocol string, body []byte) ([]byte, error) {
	if err := validateVMKernelIngress(account, body); err != nil {
		return nil, err
	}
	if protocol != "responses" {
		return body, nil
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	result := applyCodexOAuthTransformWithOptions(request, codexOAuthTransformOptions{})
	if result.Error != nil {
		return nil, result.Error
	}
	if _, ok := request["instructions"]; !ok {
		request["instructions"] = defaultCodexSynthInstructions(gjson.GetBytes(body, "model").String())
	}
	request["store"], request["stream"] = false, true
	return json.Marshal(request)
}
func (s *VMKernelService) execute(ctx context.Context, selected *Account, protocol string, body []byte, emit func(vmkernel.Event) error, probe bool) (result *vmkernel.Result, err error) {
	if s == nil || selected == nil || !selected.IsVMKernel() {
		return nil, errors.New("vm_kernel_disabled")
	}
	if protocol != "messages" && protocol != "responses" {
		return nil, errors.New("vm_protocol_mismatch")
	}
	slot := s.slot(selected.ID)
	locked := false
	defer func() {
		if locked {
			slot.mu.Unlock()
		}
	}()
	var latest *Account
	var state *vmruntime.State
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !slot.mu.TryLock() {
			// Readiness is still true during a read-only reconcile inspection. Do
			// not turn that control-plane lock contention into a false inference
			// failure. Bootstrap clears readiness; concurrent active attempts do not
			// hold this mutex or consume a second business concurrency allowance.
			if !probe && !VMKernelRuntimeReady(selected) {
				return nil, &vmkernel.AttemptError{Code: "vm_runtime_busy", SafeToRetry: true}
			}
			admissionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			acquired := slot.mu.lockContext(admissionCtx)
			cancel()
			if !acquired {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, &vmkernel.AttemptError{Code: "vm_runtime_busy", SafeToRetry: true}
			}
		}
		locked = true
		if slot.deleting || slot.revoked.Load() {
			return nil, &vmkernel.AttemptError{Code: "vm_runtime_unavailable", SafeToRetry: true}
		}
		latest, err = s.repo.GetByID(ctx, selected.ID)
		if err != nil {
			return nil, err
		}
		if !vmAccountRequestEligible(latest, probe) || vmAccountRouteRevision(latest) != vmAccountRouteRevision(selected) {
			return nil, errors.New("vm_account_unavailable")
		}
		if latest.Platform != s.platform || (protocol == "messages") != (latest.Platform == PlatformAnthropic) {
			return nil, errors.New("vm_protocol_mismatch")
		}
		// Reuse the bounded local kernel observation and applied configuration.
		// Per-request container inspection would serialize concurrent admission.
		value, found := vmKernelReadiness.Load(latest.ID)
		if !found || !VMKernelRuntimeReady(latest) {
			return nil, &vmkernel.AttemptError{Code: "vm_runtime_not_ready", SafeToRetry: true}
		}
		observed := value.(vmRuntimeStatus)
		state = &observed.state
		if !s.bindingReady(latest, state) || (slot.verifiedBinding != "" && slot.verifiedBinding != vmBindingKey(state)) {
			return nil, &vmkernel.AttemptError{Code: "vm_binding_not_ready", SafeToRetry: true}
		}
		final, err := s.repo.GetByID(ctx, latest.ID)
		if err != nil || !vmAccountRequestEligible(final, probe) || vmAccountRouteRevision(final) != vmAccountRouteRevision(latest) || vmCredentialRevision(final) != vmCredentialRevision(latest) {
			return nil, errors.New("vm_account_changed")
		}
		// Unlike scheduler leases this count includes management probes. It does
		// not reserve a second Redis slot or impose a VM-specific fixed limit.
		if final.Concurrency > 0 && slot.active >= final.Concurrency {
			if slot.activeChanged == nil {
				slot.activeChanged = make(chan struct{})
			}
			changed := slot.activeChanged
			slot.mu.Unlock()
			locked = false
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-changed:
				continue // Re-read account, credentials, proxy and generation.
			}
		}
		if bound := vmkernel.BindingFromContext(ctx); bound != nil {
			if bound.AccountID == 0 {
				*bound = vmkernel.SessionBinding{AccountID: latest.ID, InstanceID: state.InstanceID, GuestBootID: state.GuestBootID, Generation: state.Generation}
			} else if bound.AccountID != latest.ID || bound.InstanceID != state.InstanceID || bound.Generation != state.Generation || bound.GuestBootID != state.GuestBootID {
				return nil, errors.New("vm_session_generation_changed")
			}
		}
		if slot.revoked.Load() {
			return nil, errors.New("vm_account_unavailable")
		}
		if slot.active == 0 {
			slot.activeDone = make(chan struct{})
		}
		slot.active++
		slot.mu.Unlock()
		locked = false
		break
	}
	defer func() {
		slot.mu.Lock()
		defer slot.mu.Unlock()
		// An unknown request outcome forbids replay of that attempt, not other
		// requests on a healthy account. Only independent runtime observations
		// and configuration changes can trigger container recovery.
		if err != nil && result != nil && result.Dispatched && !result.Settled && !result.Completed {
			logVMKernelAttemptFailure(latest.ID, state.Generation, result, err)
		}
		slot.active--
		if slot.activeChanged != nil {
			close(slot.activeChanged)
			slot.activeChanged = nil
		}
		if slot.active == 0 {
			close(slot.activeDone)
			slot.activeDone = nil
		}
	}()
	return s.call(ctx, state, protocol, body, emit)
}
func (s *VMKernelService) call(ctx context.Context, state *vmruntime.State, protocol string, body []byte, emit func(vmkernel.Event) error) (*vmkernel.Result, error) {
	return s.kernel.Execute(ctx, vmkernel.Binding{Socket: state.KernelSocket, Token: state.InternalToken, InstanceID: state.InstanceID, Generation: state.Generation}, vmkernel.Request{Protocol: protocol, Body: body}, emit)
}
func vmAccountRouteRevision(a *Account) [32]byte {
	if a == nil {
		return [32]byte{}
	}
	ids := append([]int64(nil), a.GroupIDs...)
	slices.Sort(ids)
	models := make(map[int64][]string)
	for _, group := range a.AccountGroups {
		models[group.GroupID] = group.AllowedModels
	}
	raw, _ := json.Marshal(struct {
		Route  [32]byte
		Groups []int64
		Models map[int64][]string
	}{openAITurnRouteFingerprint(a), ids, models})
	return sha256.Sum256(raw)
}
func vmCredentialRevision(a *Account) string {
	if a == nil {
		return ""
	}
	raw, err := vmCredentialProjection(a)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func vmProxyRevision(a *Account) string {
	if a == nil || a.ProxyID == nil || a.Proxy == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", *a.ProxyID, a.Proxy.URL())))
	return hex.EncodeToString(sum[:])
}
func vmCredentialProjection(a *Account) (json.RawMessage, error) {
	token := a.GetCredential("access_token")
	if token == "" {
		return nil, errors.New("vm_access_token_missing")
	}
	if expires := a.GetCredentialAsTime("expires_at"); expires != nil && !expires.After(time.Now()) {
		return nil, errors.New("vm_access_token_expired")
	}
	if a.Platform == PlatformAnthropic {
		expires := a.GetCredentialAsTime("expires_at")
		if expires == nil {
			return nil, errors.New("vm_access_token_expiry_missing")
		}
		scopes := a.Credentials["scopes"]
		if value, ok := scopes.(string); ok {
			scopes = strings.Fields(value)
		}
		if scopes == nil {
			scopes = strings.Fields(a.GetCredential("scope"))
		}
		return json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": token, "expiresAt": expires.UnixMilli(), "scopes": scopes}})
	}
	// Sub2API owns account admission. Explicitly disable the kernel's additional
	// credential RPM/concurrency gates; do not copy account limits into a second
	// scheduler or depend on payload defaults. Provider errors still follow the
	// existing business error policy.
	return json.Marshal(map[string]any{"accounts": []any{map[string]any{
		"id": fmt.Sprint(a.ID), "access_token": token,
		"chatgpt_account_id": a.GetCredential("chatgpt_account_id"), "email": a.GetCredential("email"),
		"rpm": 0, "max_concurrency": 0,
	}}})
}

func vmTransientObservationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var runtimeError *vmruntime.Error
	if !errors.As(err, &runtimeError) {
		return false
	}
	switch runtimeError.Code {
	case "docker_operation_unknown", "helper_transport_unknown":
		return true
	}
	return false
}
