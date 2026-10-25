package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
)

var (
	ErrProxyPoolInvalid     = errors.New("proxy_pool_invalid")
	ErrProxyPoolConflict    = errors.New("proxy_pool_conflict")
	ErrProxyPoolUnavailable = errors.New("proxy_pool_unavailable")
	ErrProxyPoolEncryption  = errors.New("proxy_pool_encryption_required")
)

const ProxyPoolMaxEntries = 2000
const ProxyPoolMaxBytes = 1 << 20

// Import manages encrypted inventory without changing normal account proxies.
// Borrowing may independently opt in to persistent assignments from this pool.
type ProxyPoolEntry struct {
	ID            int64     `json:"id"`
	Protocol      string    `json:"protocol"`
	Host          string    `json:"host"`
	Port          int       `json:"port"`
	Authenticated bool      `json:"authenticated"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	Identity      string    `json:"-"`
	URL           string    `json:"-"`
}
type ProxyPoolState struct {
	Revision             int64            `json:"revision"`
	Entries              []ProxyPoolEntry `json:"entries"`
	EncryptionConfigured bool             `json:"encryption_configured"`
}
type ProxyPoolImport struct {
	Revision int64  `json:"revision"`
	Mode     string `json:"mode"`
	Text     string `json:"text"`
}
type ProxyPoolIssue struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}
type ProxyPoolPreview struct {
	Revision       int64            `json:"revision"`
	Mode           string           `json:"mode"`
	ValidCount     int              `json:"valid_count"`
	DuplicateCount int              `json:"duplicate_count"`
	AddedCount     int              `json:"added_count"`
	RetainedCount  int              `json:"retained_count"`
	RemovedCount   int              `json:"removed_count"`
	Issues         []ProxyPoolIssue `json:"issues"`
	Entries        []ProxyPoolEntry `json:"entries"`
	CanApply       bool             `json:"can_apply"`
}
type ProxyPoolRepository interface {
	Load(context.Context) (ProxyPoolState, error)
	Apply(context.Context, int64, string, []ProxyPoolEntry) (ProxyPoolState, error)
	SetEnabled(context.Context, int64, int64, bool) (ProxyPoolState, error)
}
type ProxyPoolService struct {
	repo      ProxyPoolRepository
	encryptor OpenAICredentialEncryptor
}

func NewProxyPoolService(repo ProxyPoolRepository, encryptor OpenAICredentialEncryptor) *ProxyPoolService {
	return &ProxyPoolService{repo: repo, encryptor: encryptor}
}
func (s *ProxyPoolService) State(ctx context.Context) (ProxyPoolState, error) {
	state, err := s.repo.Load(ctx)
	if err != nil {
		return state, ErrProxyPoolUnavailable
	}
	status, err := s.encryptor.EncryptionStatus()
	if err != nil {
		return state, ErrProxyPoolUnavailable
	}
	state.EncryptionConfigured = status.Configured
	return state, nil
}
func (s *ProxyPoolService) InitializeEncryption(ctx context.Context) (ProxyPoolState, error) {
	if _, err := s.encryptor.InitializeEncryption(); err != nil {
		return ProxyPoolState{}, ErrProxyPoolUnavailable
	}
	return s.State(ctx)
}
func (s *ProxyPoolService) Preview(ctx context.Context, in ProxyPoolImport) (*ProxyPoolPreview, error) {
	if (in.Mode != "append" && in.Mode != "replace") || in.Revision < 1 || len(in.Text) > ProxyPoolMaxBytes {
		return nil, ErrProxyPoolInvalid
	}
	state, err := s.State(ctx)
	if err != nil {
		return nil, err
	}
	if state.Revision != in.Revision {
		return nil, ErrProxyPoolConflict
	}
	entries, duplicates, issues := parseProxyPool(in.Text)
	p := &ProxyPoolPreview{Revision: state.Revision, Mode: in.Mode, ValidCount: len(entries), DuplicateCount: duplicates, Issues: issues, Entries: entries}
	existing := map[string]bool{}
	incoming := map[string]bool{}
	for _, e := range state.Entries {
		existing[e.Identity] = true
	}
	for _, e := range entries {
		incoming[e.Identity] = true
		if existing[e.Identity] {
			p.RetainedCount++
		} else {
			p.AddedCount++
		}
	}
	if in.Mode == "replace" {
		for key := range existing {
			if !incoming[key] {
				p.RemovedCount++
			}
		}
	}
	if len(state.Entries)+p.AddedCount-p.RemovedCount > ProxyPoolMaxEntries {
		p.Issues = append(p.Issues, ProxyPoolIssue{Reason: "pool_limit"})
	}
	p.CanApply = len(p.Issues) == 0 && len(entries) > 0 && state.EncryptionConfigured
	return p, nil
}
func (s *ProxyPoolService) Import(ctx context.Context, in ProxyPoolImport) (ProxyPoolState, error) {
	p, err := s.Preview(ctx, in)
	if err != nil {
		return ProxyPoolState{}, err
	}
	if len(p.Issues) > 0 || p.ValidCount == 0 {
		return ProxyPoolState{}, ErrProxyPoolInvalid
	}
	if !p.CanApply {
		return ProxyPoolState{}, ErrProxyPoolEncryption
	}
	state, err := s.repo.Apply(ctx, in.Revision, in.Mode, p.Entries)
	if err != nil {
		return state, proxyPoolStorageError(err)
	}
	state.EncryptionConfigured = true
	return state, nil
}
func (s *ProxyPoolService) SetEnabled(ctx context.Context, revision, id int64, enabled bool) (ProxyPoolState, error) {
	if revision < 1 || id < 1 {
		return ProxyPoolState{}, ErrProxyPoolInvalid
	}
	_, err := s.repo.SetEnabled(ctx, revision, id, enabled)
	if err != nil {
		return ProxyPoolState{}, proxyPoolStorageError(err)
	}
	return s.State(ctx)
}
func proxyPoolStorageError(err error) error {
	if errors.Is(err, ErrProxyPoolConflict) || errors.Is(err, ErrProxyPoolInvalid) {
		return err
	}
	return ErrProxyPoolUnavailable
}
func parseProxyPool(text string) ([]ProxyPoolEntry, int, []ProxyPoolIssue) {
	entries := []ProxyPoolEntry{}
	issues := []ProxyPoolIssue{}
	seen := map[string]bool{}
	duplicates := 0
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > ProxyPoolMaxEntries+1 {
		return entries, 0, []ProxyPoolIssue{{Reason: "input_limit"}}
	}
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entry, err := parseProxyPoolLine(line)
		if err != nil {
			issues = append(issues, ProxyPoolIssue{Line: i + 1, Reason: "invalid_proxy"})
			continue
		}
		if seen[entry.Identity] {
			duplicates++
			continue
		}
		seen[entry.Identity] = true
		entries = append(entries, entry)
	}
	if len(entries) == 0 && len(issues) == 0 {
		issues = append(issues, ProxyPoolIssue{Reason: "empty_input"})
	}
	return entries, duplicates, issues
}
func parseProxyPoolLine(line string) (ProxyPoolEntry, error) {
	fail := func() (ProxyPoolEntry, error) { return ProxyPoolEntry{}, ErrProxyPoolInvalid }
	if len(line) > 8192 || strings.IndexFunc(line, unicode.IsControl) >= 0 {
		return fail()
	}
	raw := line
	// A shorthand password may itself contain a URL; only a leading scheme
	// distinguishes the URL form from host:port:user:password.
	if colon := strings.IndexByte(line, ':'); colon < 0 || !strings.HasPrefix(line[colon:], "://") {
		var host, rest string
		if strings.HasPrefix(line, "[") {
			end := strings.Index(line, "]:")
			if end < 0 {
				return fail()
			}
			host = line[1:end]
			rest = line[end+2:]
		} else {
			var ok bool
			host, rest, ok = strings.Cut(line, ":")
			if !ok {
				return fail()
			}
		}
		parts := strings.SplitN(rest, ":", 3)
		if len(parts) != 1 && len(parts) != 3 {
			return fail()
		}
		u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, parts[0])}
		if len(parts) == 3 {
			u.User = url.UserPassword(parts[1], parts[2])
		}
		raw = u.String()
	}
	_, u, err := proxyurl.Parse(raw)
	if err != nil || u == nil {
		return fail()
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return fail()
	}
	host := strings.ToLower(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || host == "" || len(host) > 253 || strings.ContainsAny(host, " /\\@%") {
		return fail()
	}
	if net.ParseIP(host) == nil {
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return fail()
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return fail()
				}
			}
		}
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if u.User.Username() == "" || len(u.User.Username()) > 512 || len(password) > 512 || strings.IndexFunc(u.User.Username()+password, unicode.IsControl) >= 0 {
			return fail()
		}
		u.User = url.UserPassword(u.User.Username(), password)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = net.JoinHostPort(host, strconv.Itoa(port))
	u.Path = ""
	u.RawPath = ""
	canonical := u.String()
	sum := sha256.Sum256([]byte(canonical))
	return ProxyPoolEntry{Protocol: u.Scheme, Host: host, Port: port, Authenticated: u.User != nil, Enabled: true, Identity: hex.EncodeToString(sum[:]), URL: canonical}, nil
}
