package vmruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var revisionPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

type Helper struct {
	serviceGID int
	root       string
	driver     Driver
	lock       *os.File
	mu         sync.Mutex
	bindingMu  sync.Mutex
	networkMu  sync.Mutex
	accounts   map[int64]*sync.Mutex
	active     sync.WaitGroup
	closing    bool
}

// OpenHelper takes an exclusive process lock for the complete lifetime. The
// root must be operator-owned; it must never be shared by multiple helpers.
func OpenHelper(root string, driver Driver) (*Helper, error) {
	if !filepath.IsAbs(root) || driver == nil {
		return nil, fault("invalid_helper_configuration")
	}
	if err := privateDir(root); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(root, ".lock")
	if err := rejectSymlink(lockPath); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockStateFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Helper{root: root, driver: driver, lock: f, accounts: make(map[int64]*sync.Mutex)}, nil
}

func (h *Helper) Close() error {
	h.mu.Lock()
	h.closing = true
	h.mu.Unlock()
	h.active.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lock == nil {
		return nil
	}
	err := h.lock.Close()
	h.lock = nil
	return err
}

func (h *Helper) enter(accountID int64) (func(), error) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return nil, fault("helper_closed")
	}
	lock := h.accounts[accountID]
	if lock == nil {
		lock = &sync.Mutex{}
		h.accounts[accountID] = lock
	}
	h.active.Add(1)
	h.mu.Unlock()
	lock.Lock()
	return func() { lock.Unlock(); h.active.Done() }, nil
}

func validate(req Request) error {
	parsed, err := uuid.Parse(req.InstanceID)
	if err != nil || parsed.String() != req.InstanceID || req.AccountID <= 0 || (req.Generation == 0 && req.Operation != "inspect") || req.Version != ProtocolVersion {
		return fault("invalid_request")
	}
	if req.MigrateNetwork && req.Operation != "prepare" {
		return fault("network_migration_operation_invalid")
	}
	switch req.Operation {
	case "prepare", "apply-config", "inspect", "drain", "stop", "remove":
	default:
		return fault("unsupported_operation")
	}
	if req.Spec != nil {
		s := req.Spec
		if (s.Platform != "claude" && s.Platform != "codex") || !revisionPattern.MatchString(s.Template) || s.CPU < 1 || s.CPU > 32 || s.MemoryMiB < 512 || s.MemoryMiB > 131072 || s.DiskGiB < 4 || s.DiskGiB > 1024 || s.ProxyID <= 0 || !revisionPattern.MatchString(s.ProxyRevision) {
			return fault("invalid_spec")
		}
	}
	if req.Projection != nil {
		p := req.Projection
		if !revisionPattern.MatchString(p.Revision) || len(p.Credential) == 0 || len(p.Credential) > 256*1024 || !json.Valid(p.Credential) {
			return fault("invalid_projection")
		}
		// The helper cannot introduce a second OAuth refresh consumer.
		var doc any
		if json.Unmarshal(p.Credential, &doc) != nil || containsRefresh(doc) {
			return fault("refresh_credential_forbidden")
		}
	}
	return nil
}

func containsRefresh(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if strings.EqualFold(strings.ReplaceAll(key, "_", ""), "refreshtoken") {
				return true
			}
			if containsRefresh(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if containsRefresh(child) {
				return true
			}
		}
	}
	return false
}

func newIdentity() (Identity, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return Identity{}, err
	}
	mac := append([]byte(nil), b[:6]...)
	mac[0] = (mac[0] | 2) & 0xfe
	return Identity{UUID: uuid.NewString(), Serial: "sub2-" + hex.EncodeToString(b[:8]), MAC: fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]), DiskSerial: hex.EncodeToString(b[4:12]), MachineID: hex.EncodeToString(b), Hostname: "sub2-" + hex.EncodeToString(b[:6])}, nil
}

func (h *Helper) resolve(req Request) (Request, *State, error) {
	h.bindingMu.Lock()
	defer h.bindingMu.Unlock()
	if req.InstanceID == "" && req.AccountID > 0 {
		entries, e := os.ReadDir(h.root)
		if e != nil {
			return req, nil, e
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			old, e := h.read(filepath.Join(h.root, entry.Name()))
			if e != nil {
				return req, nil, fault("manifest_inventory_unreadable")
			}
			if old.AccountID == req.AccountID {
				req.InstanceID = old.InstanceID
				break
			}
		}
		if req.InstanceID == "" && req.Operation == "prepare" {
			req.InstanceID = uuid.NewString()
		}
	}
	if req.InstanceID == "" {
		probe := req
		probe.InstanceID = "00000000-0000-4000-8000-000000000000"
		if err := validate(probe); err != nil {
			return req, nil, err
		}
		return req, nil, fault("instance_not_found")
	}
	if err := validate(req); err != nil {
		return req, nil, err
	}
	dir := filepath.Join(h.root, req.InstanceID)
	if err := rejectSymlink(dir); err != nil {
		return req, nil, err
	}
	s, err := h.read(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return req, nil, fault("manifest_unreadable")
	}
	if s == nil {
		if req.Operation != "prepare" || req.Spec == nil {
			return req, nil, fault("instance_not_found")
		}
		// Account binding remains reserved even after a tombstone. It must never
		// accidentally attach a second guest to the same account.
		entries, err := os.ReadDir(h.root)
		if err != nil {
			return req, nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			old, e := h.read(filepath.Join(h.root, entry.Name()))
			if e != nil {
				return req, nil, fault("manifest_inventory_unreadable")
			}
			if old.AccountID == req.AccountID {
				return req, nil, fault("account_already_bound")
			}
		}
		identity, err := newIdentity()
		if err != nil {
			return req, nil, err
		}
		s = &State{AccountID: req.AccountID, InstanceID: req.InstanceID, Generation: req.Generation, Phase: "pending", Spec: *req.Spec, Identity: identity, DataRetained: true}
		if err := h.instanceDir(dir); err != nil {
			return req, nil, err
		}
		if err := h.save(dir, s); err != nil {
			return req, nil, err
		}
	}
	return req, s, nil
}

func (h *Helper) Do(ctx context.Context, req Request) (*State, error) {
	release, err := h.enter(req.AccountID)
	if err != nil {
		return nil, err
	}
	defer release()
	req, s, err := h.resolve(req)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(h.root, req.InstanceID)
	err = nil
	if s.AccountID != req.AccountID || s.InstanceID != req.InstanceID {
		return nil, fault("binding_mismatch")
	}
	if req.Generation == 0 && req.Operation == "inspect" {
		req.Generation = s.Generation
	}
	if req.Operation == "inspect" && req.Generation != s.Generation {
		return nil, fault("inspect_generation_mismatch")
	}
	if req.Generation < s.Generation {
		return nil, fault("stale_generation")
	}
	migrateLegacyNetwork := req.MigrateNetwork && s.Network == nil
	if migrateLegacyNetwork && req.Generation <= s.Generation {
		return nil, fault("network_migration_requires_new_generation")
	}
	if s.DeleteRequested && req.Operation != "inspect" && req.Operation != "remove" {
		return nil, fault("instance_deleting")
	}
	if req.Spec != nil && !reflect.DeepEqual(*req.Spec, s.Spec) {
		previousNetwork := *req.Spec
		previousNetwork.ProxyID = s.Spec.ProxyID
		previousNetwork.ProxyRevision = s.Spec.ProxyRevision
		if req.Operation != "apply-config" || req.Generation <= s.Generation || req.Projection == nil || req.Projection.ProxyURL == "" || !reflect.DeepEqual(previousNetwork, s.Spec) {
			return nil, fault("immutable_spec_mismatch")
		}
		s.Spec = *req.Spec
	}
	if req.Projection != nil && s.DesiredCredentialRevision != "" && req.Generation == s.Generation && req.Projection.Revision != s.DesiredCredentialRevision {
		return nil, fault("projection_requires_new_generation")
	}
	// Replayed prepares must not power-cycle an already ready account. Confirm
	// immutable request content before routing this retry to inspection.
	if req.Operation == "prepare" && s.Ready && req.Generation == s.Generation {
		if req.Projection != nil {
			current, e := os.ReadFile(filepath.Join(dir, "credentials", "credential.json"))
			if e != nil || sha256.Sum256(current) != sha256.Sum256(req.Projection.Credential) {
				return nil, fault("projection_revision_conflict")
			}
			if req.Projection.ProxyURL != "" {
				var proxy struct {
					URL string `json:"url"`
				}
				data, e := os.ReadFile(filepath.Join(dir, "credentials", "proxy.json"))
				if e != nil || json.Unmarshal(data, &proxy) != nil || normalizeKernelProxyURL(proxy.URL) != normalizeKernelProxyURL(req.Projection.ProxyURL) {
					return nil, fault("proxy_revision_conflict")
				}
			}
		}
		req.Operation = "inspect"
		req.Projection = nil
	}
	if (s.ContainerID != "" || s.DesiredCredentialRevision != "") && req.Operation != "inspect" && req.Operation != "remove" && req.Generation <= s.Generation {
		return nil, fault("mutation_requires_new_generation")
	}
	if req.Operation == "remove" && !s.DeleteRequested && req.Generation <= s.Generation {
		return nil, fault("mutation_requires_new_generation")
	}
	s.Capacity = 0 // Business concurrency belongs to the account scheduler.
	s.Generation = req.Generation
	if req.Operation == "remove" {
		s.DeleteRequested = true
	}
	if req.Operation != "inspect" {
		s.Ready = false
		s.Phase = req.Operation
	}
	// Persist fences and deletion intent BEFORE any external side effect.
	if err := h.save(dir, s); err != nil {
		return nil, err
	}
	fail := func(err error) (*State, error) {
		s.Ready = false
		s.Phase = "error"
		s.ErrorCode = errorCode(err)
		if e := h.save(dir, s); e != nil {
			return s, e
		}
		return s, err
	}
	if req.Operation == "prepare" || req.Operation == "apply-config" {
		if err := h.ensureClientIdentity(dir, s); err != nil {
			return fail(err)
		}
		if err := h.save(dir, s); err != nil {
			return s, err
		}
	}
	// This private operator action requires the caller to stop application
	// admission and drain all requests first. It replaces only the container;
	// instance identity, credentials, home and guest disk remain in place.
	if migrateLegacyNetwork {
		if err := h.driver.Stop(ctx, *s); err != nil {
			return fail(err)
		}
		if err := h.driver.Remove(ctx, *s); err != nil {
			return fail(err)
		}
		s.ContainerID = ""
		if err := h.save(dir, s); err != nil {
			return s, err
		}
	}
	// A legacy create may have succeeded externally before ContainerID was
	// saved. Its projected credentials distinguish it from a fresh instance:
	// this allocator always saves Network before projecting any credentials.
	// Keep such unknown legacy outcomes on their old network until explicit
	// migration confirms container removal; inspect never migrates a guest.
	allocateNetwork := s.Network != nil || migrateLegacyNetwork || (s.ContainerID == "" && s.DesiredCredentialRevision == "")
	if (req.Operation == "prepare" || req.Operation == "apply-config") && allocateNetwork {
		if err := h.reserveNetwork(ctx, s); err != nil {
			return fail(err)
		}
		if err := h.save(dir, s); err != nil {
			return s, err
		}
	}
	if req.Operation == "remove" {
		if err := h.recoverNetwork(s); err != nil {
			return fail(err)
		}
		if err := h.save(dir, s); err != nil {
			return s, err
		}
	}
	if err := h.runDir(filepath.Join(dir, "run")); err != nil {
		return fail(err)
	}
	tokenPath := filepath.Join(dir, "run", "internal.token")
	if err := rejectSymlink(tokenPath); err != nil {
		return fail(err)
	}
	token, e := os.ReadFile(tokenPath)
	if errors.Is(e, os.ErrNotExist) {
		if s.ContainerID != "" || s.DesiredCredentialRevision != "" {
			return fail(fault("internal_token_missing"))
		}
		b := make([]byte, 32)
		if _, e = rand.Read(b); e != nil {
			return fail(e)
		}
		token = []byte(hex.EncodeToString(b))
		if e = atomicBytes(tokenPath, token); e != nil {
			return fail(e)
		}
	} else if e != nil {
		return fail(e)
	}
	if len(token) != 64 {
		return fail(fault("internal_token_corrupt"))
	}
	s.InternalToken = string(token)
	if req.Projection != nil {
		if req.Operation != "prepare" && req.Operation != "apply-config" {
			return fail(fault("projection_operation_invalid"))
		}
		if s.ContainerID != "" || s.DesiredCredentialRevision != "" {
			if err := h.driver.Stop(ctx, *s); err != nil {
				return fail(err)
			}
		}
		if err := h.project(dir, s, *req.Projection); err != nil {
			return fail(err)
		}
	}
	var observation Observation
	switch req.Operation {
	case "prepare":
		if s.DesiredCredentialRevision == "" {
			return fail(fault("credential_projection_required"))
		}
		if err := h.driver.Preflight(ctx, *s); err != nil {
			return fail(err)
		}
		var id string
		id, err = h.driver.Ensure(ctx, *s, dir)
		if err != nil {
			return fail(err)
		}
		s.ContainerID = id
		if err := h.save(dir, s); err != nil {
			return s, err
		}
		observation, err = h.driver.Apply(ctx, *s, dir)
	case "apply-config":
		if err := h.driver.Preflight(ctx, *s); err != nil {
			return fail(err)
		}
		if req.Projection == nil {
			return fail(fault("credential_projection_required"))
		}
		id, ensureErr := h.driver.Ensure(ctx, *s, dir)
		if ensureErr != nil {
			return fail(ensureErr)
		}
		s.ContainerID = id
		if err := h.save(dir, s); err != nil {
			return s, err
		}
		observation, err = h.driver.Apply(ctx, *s, dir)
	case "inspect":
		observation, err = h.driver.Inspect(ctx, *s, dir)
	case "drain", "stop":
		err = h.driver.Stop(ctx, *s)
		s.Phase = "stopped"
	case "remove":
		if err = h.driver.Stop(ctx, *s); err == nil {
			err = h.driver.Remove(ctx, *s)
		}
		if err == nil {
			err = h.releaseNetwork(s)
		}
		if err == nil {
			s.Phase = "removed"
			s.ContainerID = ""
		}
	}
	if err != nil {
		return fail(err)
	}
	if req.Operation == "prepare" || req.Operation == "apply-config" || req.Operation == "inspect" {
		s.GuestBootID = observation.GuestBootID
		s.AppliedCredentialRevision = observation.CredentialRevision
		s.AppliedProxyRevision = observation.ProxyRevision
		s.ActualAccelerator = observation.ActualAccelerator
		acceleratorOK := observation.KVMEnabled && (observation.ActualAccelerator == "" || observation.ActualAccelerator == "kvm")
		if policy, ok := h.driver.(interface{ AllowsTCG(State) bool }); ok && policy.AllowsTCG(*s) {
			acceleratorOK = observation.ActualAccelerator == "tcg" && !observation.KVMEnabled
		}
		s.Ready = !s.DeleteRequested && observation.Running && acceleratorOK && observation.KernelReady && observation.GuestBootID != "" && observation.CredentialRevision == s.DesiredCredentialRevision && observation.ProxyRevision == s.Spec.ProxyRevision
		s.Phase = "preparing"
		if s.Ready {
			s.Phase = "kernel_ready"
		}
		if !observation.Running {
			s.Phase = "stopped"
		}
		if s.DeleteRequested {
			s.Phase = "removed"
			if s.ContainerID != "" {
				s.Phase = "deleting"
			}
		}
		s.KernelSocket = filepath.Join(dir, "run", "kernel.sock")
	}
	s.ErrorCode = ""
	if err := h.save(dir, s); err != nil {
		return s, err
	}
	return s, nil
}

func errorCode(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return "runtime_operation_failed"
}
func (h *Helper) read(dir string) (*State, error) {
	p := filepath.Join(dir, "manifest.json")
	if err := rejectSymlink(p); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var s State
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.InstanceID == "" || s.AccountID <= 0 || s.Identity.UUID == "" {
		return nil, fault("manifest_invalid")
	}
	return &s, nil
}
func (h *Helper) save(dir string, s *State) error {
	s.UpdatedAt = time.Now().UTC()
	copy := *s
	copy.InternalToken = ""
	return atomicJSON(filepath.Join(dir, "manifest.json"), copy)
}
func (h *Helper) project(dir string, s *State, p Projection) error {
	projectionDir := filepath.Join(dir, "credentials")
	if err := privateDir(projectionDir); err != nil {
		return err
	}
	if p.ProxyURL != "" {
		u, e := url.Parse(p.ProxyURL)
		if e != nil || u.Hostname() == "" || u.Port() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return fault("proxy_url_invalid")
		}
		addresses, e := net.LookupIP(u.Hostname())
		if e != nil {
			return fault("proxy_resolution_failed")
		}
		ips := []string{}
		for _, ip := range addresses {
			if ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() {
				ips = append(ips, ip.String())
			}
		}
		if len(ips) == 0 {
			return fault("proxy_ipv4_required")
		}
		projection := struct {
			URL      string   `json:"url"`
			IPs      []string `json:"ips"`
			Port     string   `json:"port"`
			Revision string   `json:"revision"`
		}{normalizeKernelProxyURL(p.ProxyURL), ips, u.Port(), s.Spec.ProxyRevision}
		if e = atomicJSON(filepath.Join(projectionDir, "proxy.json"), projection); e != nil {
			return e
		}
	} else if _, e := os.Stat(filepath.Join(projectionDir, "proxy.json")); e != nil {
		return fault("proxy_projection_required")
	}
	sum := sha256.Sum256(p.Credential)
	stamp := struct {
		Revision string `json:"revision"`
		Digest   string `json:"digest"`
	}{p.Revision, hex.EncodeToString(sum[:])}
	stampPath := filepath.Join(projectionDir, "revision.json")
	if old, err := os.ReadFile(stampPath); err == nil {
		var previous struct {
			Revision string `json:"revision"`
			Digest   string `json:"digest"`
		}
		if json.Unmarshal(old, &previous) != nil {
			return fault("projection_revision_corrupt")
		}
		if previous.Revision == p.Revision && previous.Digest != stamp.Digest {
			return fault("projection_revision_conflict")
		}
	}
	// The driver drains/stops the guest kernel before application. Files are
	// atomically replaced within a directory-mounted read-only guest share.
	if err := atomicBytes(filepath.Join(projectionDir, "credential.json"), p.Credential); err != nil {
		return err
	}
	if err := atomicJSON(stampPath, stamp); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		for _, path := range []string{projectionDir, filepath.Join(projectionDir, "credential.json"), stampPath, filepath.Join(projectionDir, "proxy.json")} {
			if err := os.Chown(path, 10001, 10001); err != nil {
				return err
			}
		}
	}
	s.DesiredCredentialRevision = p.Revision
	return h.save(dir, s)
}
func rejectSymlink(p string) error {
	st, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fault("symlink_forbidden")
	}
	return nil
}
func privateDir(p string) error {
	if err := rejectSymlink(p); err != nil {
		return err
	}
	if err := os.MkdirAll(p, 0700); err != nil {
		return err
	}
	return os.Chmod(p, 0700)
}
func atomicJSON(p string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicBytes(p, b)
}
func atomicBytes(p string, b []byte) error {
	return atomicBytesOwned(p, b, -1, -1)
}

// Ownership is applied before rename so the reader never sees a root-only replacement.
func atomicBytesOwned(p string, b []byte, uid, gid int) error {
	if err := rejectSymlink(p); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".write-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if uid >= 0 {
		if err = f.Chown(uid, gid); err != nil {
			f.Close()
			return err
		}
	}
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, p); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(p))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// VerifyArtifact never downloads or executes a payload.
func VerifyArtifact(path, digest string) error {
	if len(digest) != 64 {
		return fault("artifact_digest_invalid")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fault("artifact_digest_invalid")
	}
	if err := rejectSymlink(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fault("artifact_missing")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return fault("artifact_not_regular")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return fault("artifact_digest_mismatch")
	}
	return nil
}

// Inventory returns persisted private lifecycle metadata, never credentials or
// internal tokens. It does not inspect Docker or attest current readiness.
func (h *Helper) Inventory() ([]State, error) {
	release, err := h.enter(0)
	if err != nil {
		return nil, err
	}
	defer release()
	h.bindingMu.Lock()
	defer h.bindingMu.Unlock()
	entries, err := os.ReadDir(h.root)
	if err != nil {
		return nil, fault("manifest_inventory_unreadable")
	}
	states := make([]State, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := h.read(filepath.Join(h.root, entry.Name()))
		if err != nil {
			return nil, fault("manifest_inventory_unreadable")
		}
		state.InternalToken = ""
		state.Ready = false
		states = append(states, *state)
	}
	return states, nil
}

// SOCKS target DNS must be resolved by the proxy: the guest intentionally has
// no direct DNS egress. Account configuration itself retains its original URL.
func normalizeKernelProxyURL(raw string) string {
	u, e := url.Parse(raw)
	if e == nil && u.Scheme == "socks5" {
		u.Scheme = "socks5h"
		return u.String()
	}
	return raw
}
