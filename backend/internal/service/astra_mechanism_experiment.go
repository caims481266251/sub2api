//go:build astra_experiment

package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
)

// Opt-in diagnostic bridge. Normal server builds do not expose these helpers.
// Both native and borrowed arms use the same payload, retries and verdict code.
type AstraExperimentShot struct {
	Phase                string             `json:"phase"`
	Status               int                `json:"status"`
	DurationMS           int64              `json:"duration_ms"`
	Failure              string             `json:"failure,omitempty"`
	TicketHash           string             `json:"ticket_hash,omitempty"`
	TicketLength         int                `json:"ticket_length"`
	SentRouteHash        string             `json:"sent_route_hash,omitempty"`
	ResponseRouteHash    string             `json:"response_route_hash,omitempty"`
	SentBalancerHash     string             `json:"sent_balancer_hash,omitempty"`
	ResponseBalancerHash string             `json:"response_balancer_hash,omitempty"`
	ResponseHost         string             `json:"response_host,omitempty"`
	RouteChanged         bool               `json:"route_changed"`
	Protocol             string             `json:"protocol,omitempty"`
	ConnectionHash       string             `json:"connection_hash,omitempty"`
	ConnectionReused     bool               `json:"connection_reused"`
	Limits               map[string]float64 `json:"limits,omitempty"`
}
type AstraExperimentResult struct {
	Result *OpenAICodexStateProbeResult `json:"result"`
	Shots  []AstraExperimentShot        `json:"shots"`
}

func AstraExperimentHash(value string) string {
	if value == "" {
		return ""
	}
	s := sha256.Sum256([]byte(value))
	return hex.EncodeToString(s[:])
}

// Expose only the normalized wait, never raw upstream response headers.
func AstraExperimentRetryAfter(header http.Header) int64 {
	delay := borrowRetryAfter(header.Get("Retry-After"), time.Now())
	return int64((delay + time.Second - 1) / time.Second)
}
func AstraExperimentHost(value string) string {
	p := strings.Split(value, ".")
	if len(p) != 3 {
		return ""
	}
	raw, e := base64.RawURLEncoding.DecodeString(p[1])
	if e != nil {
		return ""
	}
	var v struct {
		Host string `json:"host"`
	}
	json.Unmarshal(raw, &v)
	if strings.HasPrefix(v.Host, "chat.gateway.unified-") && strings.HasSuffix(v.Host, ".api.openai.com") {
		return v.Host
	}
	return ""
}
func QualifyAstraExperimentSource(body []byte, model string) error {
	return validateCodexProbeResponse(body, model)
}

// The allowlist admits numeric quota observations only, never response secrets.
func AstraExperimentLimits(header http.Header) map[string]float64 {
	limits := map[string]float64{}
	for _, window := range []string{"primary", "secondary"} {
		for _, metric := range []string{"used-percent", "window-minutes", "reset-after-seconds"} {
			name := "x-codex-" + window + "-" + metric
			if value, err := strconv.ParseFloat(header.Get(name), 64); err == nil && value >= 0 && value < 1e12 {
				limits[name] = value
			}
		}
	}
	return limits
}

type AstraExperimentOptions struct {
	StableSession bool
	KeepAlive     bool
	Model         string
	SeedBalancer  *http.Cookie
}

func ProbeAstraExperiment(ctx context.Context, upstream HTTPUpstream, template *http.Request, proxy string, id int64, profile *tlsfingerprint.Profile, seed *http.Cookie, stableSession bool) AstraExperimentResult {
	return ProbeAstraExperimentWithOptions(ctx, upstream, template, proxy, id, profile, seed, AstraExperimentOptions{StableSession: stableSession})
}

func ProbeAstraExperimentWithOptions(ctx context.Context, upstream HTTPUpstream, template *http.Request, proxy string, id int64, profile *tlsfingerprint.Profile, seed *http.Cookie, options AstraExperimentOptions) AstraExperimentResult {
	result := &OpenAICodexStateProbeResult{AccountID: id, Model: OpenAICodexStateProbeDefaultModel, Verdict: OpenAICodexStateInconclusive, StartedAt: time.Now()}
	out := AstraExperimentResult{Result: result, Shots: []AstraExperimentShot{}}
	if upstream == nil || template == nil {
		result.fail("unsupported", "missing experiment transport", "")
		return out
	}
	if options.Model != "" {
		if options.Model != OpenAICodexStateProbeDefaultModel && options.Model != "gpt-6-luna" {
			result.fail("unsupported", "unsupported experimental control model", "")
			return out
		}
		result.Model = options.Model
	}
	pinned := ""
	if seed != nil {
		// Acquisition returns a Set-Cookie value with response-only attributes.
		// A request Cookie must contain only the cookie name and value.
		pinned = (&http.Cookie{Name: "__oailb", Value: seed.Value}).String()
	}
	session := uuid.NewString()
	var balancer *http.Cookie
	if options.SeedBalancer != nil && options.SeedBalancer.Name == "__cflb" && options.SeedBalancer.Value != "" {
		balancer = &http.Cookie{Name: "__cflb", Value: options.SeedBalancer.Value}
	}
	routeChanged := false
	runOpenAICodexStateProbe(ctx, result, pinned, func(state, cookie string) (openAICodexStateShot, error) {
		return retryOpenAICodexStateRouteShot(ctx, result, state == "", func() (openAICodexStateShot, error) {
			start := time.Now()
			obs := AstraExperimentShot{Phase: "mint"}
			if state != "" {
				obs.Phase = "continue"
			}
			shot, err := fireOpenAICodexStateShotRequest(ctx, template.Header, result.Model, state, cookie, func(req *http.Request) (*http.Response, error) {
				req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(req.Context()))
				if balancer != nil {
					if _, err := req.Cookie("__cflb"); err != nil {
						req.AddCookie(balancer)
					}
				}
				if options.StableSession {
					req.Header.Set("session_id", session)
				}
				if options.KeepAlive {
					req.Close = false
					req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
				}
				req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
					obs.ConnectionHash = AstraExperimentHash(fmt.Sprintf("%p", info.Conn))
					obs.ConnectionReused = info.Reused
				}}))
				sent, _ := req.Cookie("__oailb")
				if balancer, e := req.Cookie("__cflb"); e == nil {
					obs.SentBalancerHash = AstraExperimentHash(balancer.Value)
				}
				if sent != nil {
					obs.SentRouteHash = AstraExperimentHash(sent.Value)
				}
				resp, e := upstream.DoWithTLS(req, proxy, id, 1, profile)
				if resp != nil {
					obs.Status = resp.StatusCode
					obs.Protocol = resp.Proto
					obs.Limits = AstraExperimentLimits(resp.Header)
					for _, got := range resp.Cookies() {
						if got.Name == "__cflb" {
							obs.ResponseBalancerHash = AstraExperimentHash(got.Value)
							if options.SeedBalancer != nil {
								balancer = nil
								if got.Value != "" && got.MaxAge >= 0 && (got.Expires.IsZero() || time.Now().Before(got.Expires)) {
									balancer = &http.Cookie{Name: "__cflb", Value: got.Value}
								}
							}
						}
						if got.Name == "__oailb" {
							obs.ResponseRouteHash = AstraExperimentHash(got.Value)
							obs.ResponseHost = AstraExperimentHost(got.Value)
							if sent != nil && (got.Value != sent.Value || got.MaxAge < 0 || (!got.Expires.IsZero() && !time.Now().Before(got.Expires))) {
								obs.RouteChanged = true
								routeChanged = true
							}
						}
					}
				}
				if routeChanged {
					if resp != nil && resp.Body != nil {
						resp.Body.Close()
					}
					return nil, errOpenAICodexStateRouteChanged
				}
				return resp, e
			})
			obs.DurationMS = time.Since(start).Milliseconds()
			obs.TicketHash = AstraExperimentHash(shot.state)
			obs.TicketLength = len(shot.state)
			check := &OpenAICodexStateProbeResult{}
			if !check.shotUsable(ctx, "", shot, err) {
				obs.Failure = check.Failure
			}
			if errors.Is(err, errOpenAICodexStateRouteChanged) {
				obs.Failure = "route_changed"
			}
			out.Shots = append(out.Shots, obs)
			return shot, err
		})
	})
	if routeChanged {
		result.fail("route_changed", "route changed", "")
	}
	result.FinishedAt = time.Now()
	result.LatencyMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
	if result.Reason == "" {
		result.Reason = openAICodexStateVerdictReason(result.Verdict)
	}
	// Diagnostics only expose typed failure codes; never retain upstream error text.
	result.Detail = ""
	return out
}
