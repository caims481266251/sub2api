package vmruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"time"
)

var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// ReleaseOperation contacts only the operator-installed local update agent.
// The unprivileged application never receives Docker or systemd access.
func ReleaseOperation(ctx context.Context, socket, action, version string) error {
	if !releaseVersion.MatchString(version) || (action != "prepare" && action != "activate") {
		return fmt.Errorf("runtime_release_invalid")
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket+".update")
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 12 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	data, _ := json.Marshal(map[string]string{"version": version})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://runtime-updater/v1/"+action, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Docker runtime update agent unavailable; install the matching release host bundle before updating: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Error   string `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out) != nil {
		return fmt.Errorf("runtime_update_response_invalid")
	}
	if resp.StatusCode != 200 || !out.OK || out.Version != version {
		return fmt.Errorf("Docker runtime %s failed: %s", action, out.Error)
	}
	return nil
}
