//go:build astra_experiment

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Diagnostic-only paired control. It reuses a published production route and
// never publishes, renews, or relaxes that route's admission deadline.
type AstraBusinessControlResult struct {
	Result   *OpenAICodexStateProbeResult `json:"result"`
	Shots    []AstraBusinessControlShot   `json:"shots"`
	Business *AstraExitBusinessResult     `json:"business,omitempty"`
}
type AstraExitBusinessResult struct {
	Complete     bool   `json:"complete"`
	NewTicket    bool   `json:"new_ticket"`
	RouteChanged bool   `json:"route_changed"`
	Failure      string `json:"failure,omitempty"`
}
type AstraBusinessControlShot struct {
	AstraExperimentShot
	BodyHash          string                `json:"body_hash"`
	Text              string                `json:"text,omitempty"`
	StartedAt         time.Time             `json:"started_at"`
	FinishedAt        time.Time             `json:"finished_at"`
	RetryAfterSeconds int64                 `json:"retry_after_seconds,omitempty"`
	Stream            AstraDiagnosticStream `json:"stream"`
}

func ProbeAstraBusinessControl(ctx context.Context, repo TicketBorrowRepository, upstream HTTPUpstream, route *BorrowRoute, label, prompt string, guard func(context.Context) error) AstraBusinessControlResult {
	return probeAstraBusinessControl(ctx, repo, upstream, route, label, prompt, false, guard)
}

// ProbeAstraExitControl runs the production short verdict, then exactly one
// immediate candy request on its own freshly minted ticket when healthy.
// It never publishes its candidate or changes the normal account environment.
func ProbeAstraExitControl(ctx context.Context, repo TicketBorrowRepository, upstream HTTPUpstream, route *BorrowRoute, label, prompt string, guard func(context.Context) error) AstraBusinessControlResult {
	return probeAstraBusinessControl(ctx, repo, upstream, route, label, prompt, true, guard)
}

func probeAstraBusinessControl(ctx context.Context, repo TicketBorrowRepository, upstream HTTPUpstream, route *BorrowRoute, label, prompt string, candyAfter bool, guard func(context.Context) error) AstraBusinessControlResult {
	result := &OpenAICodexStateProbeResult{AccountID: route.TargetID, Model: OpenAICodexStateProbeDefaultModel, Verdict: OpenAICodexStateInconclusive, StartedAt: time.Now()}
	out := AstraBusinessControlResult{Result: result, Shots: []AstraBusinessControlShot{}}
	tracker := &TicketBorrowService{repo: repo}
	routeChanged := false
	seed := route.Secret.Headers.Clone()
	if seed == nil {
		seed = make(http.Header)
	}
	seed.Del(openAICodexTurnStateHeader)
	replaceCodexWSAnchorCookie(seed, route.Secret.Cookie.Value)
	var minted string
	fire := func(state, cookie, bodyPrompt string, candy bool) (openAICodexStateShot, error) {
		started := time.Now()
		obs := AstraBusinessControlShot{AstraExperimentShot: AstraExperimentShot{Phase: "mint"}, StartedAt: started}
		if state != "" {
			obs.Phase = "continue"
		}
		if candy {
			obs.Phase = "candy"
		}
		var captured bytes.Buffer
		shot, err := fireOpenAICodexStateShotRequest(ctx, seed, result.Model, state, cookie, func(req *http.Request) (*http.Response, error) {
			if err := guard(req.Context()); err != nil {
				return nil, err
			}
			if bodyPrompt != "" {
				body, _ := json.Marshal(map[string]any{"model": result.Model, "instructions": "请解答题目，给出最少数量和论证。", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": bodyPrompt}}}}, "stream": true, "store": false})
				_ = req.Body.Close()
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
			}
			body, _ := req.GetBody()
			data, _ := io.ReadAll(body)
			_ = body.Close()
			obs.BodyHash = AstraExperimentHash(string(data))
			obs.SentRouteHash = AstraExperimentHash(route.Secret.Cookie.Value)
			if lb, e := req.Cookie("__cflb"); e == nil {
				obs.SentBalancerHash = AstraExperimentHash(lb.Value)
			}
			req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(req.Context()))
			req = req.WithContext(context.WithValue(req.Context(), borrowBeforeSendKey{}, guard))
			q := BorrowRequest{Kind: "business", TargetID: route.TargetID, SourceID: route.SourceID, RouteID: route.ID, BatchID: label, Phase: "diagnostic_" + obs.Phase}
			resp, err := tracker.trackTargetRequest(req, q, route.Secret.Proxy, route.TargetID, 1, route.Secret.Profile, upstream.DoWithTLS)
			if resp != nil {
				obs.Status, obs.Protocol = resp.StatusCode, resp.Proto
				if resp.StatusCode == http.StatusTooManyRequests {
					obs.RetryAfterSeconds = AstraExperimentRetryAfter(resp.Header)
				}
				for _, c := range resp.Cookies() {
					if c.Name == "__oailb" {
						obs.ResponseRouteHash = AstraExperimentHash(c.Value)
						obs.ResponseHost = borrowGateway(c.Value)
						if c.Value != route.Secret.Cookie.Value || c.MaxAge < 0 || (!c.Expires.IsZero() && !time.Now().Before(c.Expires)) {
							routeChanged = true
						}
						if !routeChanged {
							ttl := time.Until(route.ExpiresAt)
							if c.MaxAge > 0 {
								ttl = min(ttl, time.Duration(c.MaxAge)*time.Second)
							}
							if !c.Expires.IsZero() {
								ttl = min(ttl, time.Until(c.Expires))
							}
							if _, e := repo.ObserveCookie(req.Context(), route.CookieHash, ttl); e != nil {
								if resp.Body != nil {
									_ = resp.Body.Close()
								}
								return nil, e
							}
						}
					}
					if c.Name == "__cflb" {
						obs.ResponseBalancerHash = AstraExperimentHash(c.Value)
					}
				}
				obs.RouteChanged = routeChanged
				if routeChanged {
					if resp.Body != nil {
						_ = resp.Body.Close()
					}
					return nil, errOpenAICodexStateRouteChanged
				}
				if resp.Body != nil {
					resp.Body = &astraCaptureBody{ReadCloser: resp.Body, dst: &captured}
				}
			}
			return resp, err
		})
		obs.TicketHash, obs.TicketLength = AstraExperimentHash(shot.state), len(shot.state)
		obs.Text = astraBusinessControlText(captured.Bytes())
		obs.Stream = astraDiagnosticStream(captured.Bytes())
		obs.FinishedAt, obs.DurationMS = time.Now(), time.Since(started).Milliseconds()
		check := &OpenAICodexStateProbeResult{}
		if !check.shotUsable(ctx, "", shot, err) {
			obs.Failure = check.Failure
		}
		out.Shots = append(out.Shots, obs)
		return shot, err
	}
	runOpenAICodexStateProbe(ctx, result, seed.Get("Cookie"), func(state, cookie string) (openAICodexStateShot, error) {
		bodyPrompt := prompt
		if candyAfter {
			bodyPrompt = ""
		}
		shot, err := fire(state, cookie, bodyPrompt, false)
		if state == "" {
			minted = shot.state
		}
		return shot, err
	})
	if routeChanged {
		result.fail("route_changed", "route changed", "")
	}
	if candyAfter && result.Verdict == OpenAICodexStateHealthy && result.Failure == "" {
		shot, err := fire(minted, result.RouteCookie, prompt, true)
		check := &OpenAICodexStateProbeResult{}
		complete := check.shotUsable(ctx, "", shot, err)
		out.Business = &AstraExitBusinessResult{Complete: complete, NewTicket: shot.state != "" && shot.state != minted, RouteChanged: routeChanged, Failure: check.Failure}
	}
	result.Detail = ""
	result.FinishedAt = time.Now()
	result.LatencyMs = time.Since(result.StartedAt).Milliseconds()
	return out
}

type astraCaptureBody struct {
	io.ReadCloser
	dst *bytes.Buffer
}

func (b *astraCaptureBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if b.dst.Len() < openAICodexStateProbeMaxBody {
		_, _ = b.dst.Write(p[:min(n, openAICodexStateProbeMaxBody-b.dst.Len())])
	}
	return n, e
}
func astraBusinessControlText(data []byte) string {
	var text bytes.Buffer
	for _, payload := range openAICodexStateStreamEvents(data) {
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal(payload, &event) == nil && event.Type == "response.output_text.delta" {
			text.WriteString(event.Delta)
		}
	}
	return text.String()
}
