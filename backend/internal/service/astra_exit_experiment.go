//go:build astra_experiment

package service

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// This diagnostic acquisition uses the production minimal request and source
// fingerprint generator. Account/selection/exit guards are supplied by the CLI.
// Its Cookie stays in memory; only allowlisted metadata is serializable.
type AstraExitSourceResult struct {
	Cookie            *http.Cookie          `json:"-"`
	ExpiresAt         time.Time             `json:"expires_at"`
	ObservedAt        time.Time             `json:"observed_at"`
	CookieHash        string                `json:"cookie_hash,omitempty"`
	Gateway           string                `json:"gateway,omitempty"`
	Status            int                   `json:"status"`
	Complete          bool                  `json:"complete"`
	Failure           string                `json:"failure,omitempty"`
	RetryAfterSeconds int64                 `json:"retry_after_seconds"`
	Environment       BorrowEnvironment     `json:"environment"`
	Stream            AstraDiagnosticStream `json:"stream"`
}

func AcquireAstraExitCandidate(ctx context.Context, repo TicketBorrowRepository, up HTTPUpstream, headers http.Header, proxy string, sourceID int64, ttl int, guard func(context.Context) error) AstraExitSourceResult {
	var out AstraExitSourceResult
	var captured bytes.Buffer
	device := uuid.NewString()
	headers = headers.Clone()
	for _, k := range []string{"Cookie", openAICodexTurnStateHeader, "session_id", "conversation_id"} {
		headers.Del(k)
	}
	headers.Set("X-Codex-Installation-ID", device)
	headers.Set("X-Codex-Window-ID", device)
	profile := borrowSourceProfile(device)
	shot, err := fireOpenAICodexStateShotRequest(ctx, headers, "gpt-6-luna", "", "", func(req *http.Request) (*http.Response, error) {
		if err := guard(req.Context()); err != nil {
			return nil, err
		}
		req = req.WithContext(context.WithValue(req.Context(), borrowBeforeSendKey{}, guard))
		req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(req.Context()))
		out.Environment = borrowEnvironment(req, proxy, profile)
		resp, err := up.DoWithTLS(req, proxy, sourceID, 1, profile)
		if resp != nil {
			out.Status = resp.StatusCode
			if resp.StatusCode == http.StatusTooManyRequests {
				out.RetryAfterSeconds = AstraExperimentRetryAfter(resp.Header)
			}
			if resp.Body != nil {
				resp.Body = &astraCaptureBody{ReadCloser: resp.Body, dst: &captured}
			}
			for _, c := range resp.Cookies() {
				if c.Name == "__oailb" {
					copy := *c
					out.Cookie = &copy
					out.ObservedAt = time.Now()
				}
			}
		}
		return resp, err
	})
	check := &OpenAICodexStateProbeResult{}
	out.Complete = check.shotUsable(ctx, "", shot, err)
	out.Failure = check.Failure
	out.Stream = astraDiagnosticStream(captured.Bytes())
	if !out.Complete || out.Cookie == nil || out.Cookie.MaxAge < 0 || borrowGateway(out.Cookie.Value) == "" {
		out.Cookie = nil
		return out
	}
	lifetime := borrowCookieTTL(out.Cookie, out.ObservedAt, time.Now(), ttl)
	if lifetime <= 0 {
		out.Cookie = nil
		out.Failure = "expired_cookie"
		return out
	}
	out.CookieHash = borrowHash(out.Cookie.Value)
	out.Gateway = borrowGateway(out.Cookie.Value)
	out.ExpiresAt, err = repo.ObserveCookie(ctx, out.CookieHash, lifetime)
	if err != nil {
		out.Cookie = nil
		out.Failure = "cookie_store_failed"
	}
	return out
}
