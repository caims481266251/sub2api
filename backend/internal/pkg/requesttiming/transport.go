package requesttiming

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
)

// TransportInfo describes the selected transport, not the observed public IP.
// IDs are digests; no URL, credential, header value or payload is retained.
type TransportInfo struct {
	Kind          string `json:"kind,omitempty"`
	ProxyMode     string `json:"proxy_mode,omitempty"`
	PoolID        string `json:"pool_id,omitempty"`
	FingerprintID string `json:"fingerprint_id,omitempty"`
	IdentityID    string `json:"identity_id,omitempty"`
}

type transportKey struct{}

// WithTransport runs at the final egress boundary, after route/plugin selection.
// It also updates an already-started physical attempt (regional routing).
func WithTransport(ctx context.Context, info TransportInfo) context.Context {
	if From(ctx) == nil {
		return ctx
	}
	if tr, ok := ctx.Value(attemptKey{}).(*Trace); ok {
		tr.update(func(a *Attempt) { a.Transport = info })
	}
	return context.WithValue(ctx, transportKey{}, info)
}

// IdentityDigest intentionally excludes auth, cookies and per-request metadata.
// It describes only the application identity headers actually being sent.
func IdentityDigest(headers http.Header) string {
	h := sha256.New()
	found := false
	for _, name := range []string{"User-Agent", "Originator", "Version", "X-Codex-Installation-Id"} {
		value := headers.Get(name)
		found = found || value != ""
		_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
	}
	if !found {
		return ""
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// TransportStage observes a bounded set of local transport operations. SOCKS
// setup includes dialing the proxy; it must not be presented as pure tunnel RTT.
func TransportStage(ctx context.Context, stage string) func() {
	switch stage {
	case "client_acquire", "proxy_tls", "proxy_connect", "socks_setup":
	default:
		return func() {}
	}
	tr, _ := ctx.Value(attemptKey{}).(*Trace)
	if tr == nil {
		return func() {}
	}
	tr.mark(stage + "_start")
	var once sync.Once
	return func() { once.Do(func() { tr.mark(stage + "_end") }) }
}
