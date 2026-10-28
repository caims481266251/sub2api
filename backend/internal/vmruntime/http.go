package vmruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"
)

type inventoryReply struct {
	States []State `json:"states"`
	Error  *Error  `json:"error,omitempty"`
}

type systemProfilesReply struct {
	Profiles []SystemProfile `json:"profiles"`
	Error    *Error          `json:"error,omitempty"`
}

type reply struct {
	State *State `json:"state,omitempty"`
	Error *Error `json:"error,omitempty"`
}

func (h *Helper) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/version" && r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"version": h.Version, "runtime": "container-v1", "protocol": ProtocolVersion})
			return
		}
		if r.URL.Path == "/v1/system-profiles" && r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(systemProfilesReply{Profiles: h.SystemProfiles()})
			return
		}
		if r.URL.Path == "/v1/inventory" && r.Method == http.MethodGet {
			states, err := h.Inventory()
			if err != nil {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(inventoryReply{Error: &Error{Code: errorCode(err)}})
				return
			}
			json.NewEncoder(w).Encode(inventoryReply{States: states})
			return
		}
		if r.URL.Path != "/v1/runtime" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 320*1024)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var req Request
		if err := dec.Decode(&req); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(reply{Error: &Error{Code: "invalid_json"}})
			return
		}
		var trailing any
		if dec.Decode(&trailing) != io.EOF {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(reply{Error: &Error{Code: "invalid_json"}})
			return
		}
		s, err := h.Do(r.Context(), req)
		if err != nil {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(reply{State: s, Error: &Error{Code: errorCode(err)}})
			return
		}
		json.NewEncoder(w).Encode(reply{State: s})
	})
}

type Client struct{ http *http.Client }

// NewClient always dials the configured local Unix socket. Requests cannot
// select an endpoint, proxy, URL, or redirect target.
func NewClient(socket string) *Client {
	t := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{http: &http.Client{Transport: t, Timeout: 11 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }
func (c *Client) Do(ctx context.Context, req Request) (*State, error) {
	if req.Version == 0 {
		req.Version = ProtocolVersion
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://runtime-helper/v1/runtime", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(r)
	if err != nil {
		return nil, fault("helper_transport_unknown")
	}
	defer res.Body.Close()
	var out reply
	if err = json.NewDecoder(io.LimitReader(res.Body, 1024*1024)).Decode(&out); err != nil {
		return nil, fault("helper_response_invalid")
	}
	if out.Error != nil {
		return out.State, out.Error
	}
	if res.StatusCode != 200 || out.State == nil {
		return nil, fault("helper_response_invalid")
	}
	return out.State, nil
}

// Inventory reads only the private helper UDS. Entries are lifecycle metadata,
// not readiness attestations; call Do(inspect) before admitting any request.
func (c *Client) Inventory(ctx context.Context) ([]State, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime-helper/v1/inventory", nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(r)
	if err != nil {
		return nil, fault("helper_transport_unknown")
	}
	defer res.Body.Close()
	var out inventoryReply
	if err := json.NewDecoder(io.LimitReader(res.Body, 16*1024*1024)).Decode(&out); err != nil {
		return nil, fault("helper_response_invalid")
	}
	if out.Error != nil {
		return nil, out.Error
	}
	if res.StatusCode != http.StatusOK || out.States == nil {
		return nil, fault("helper_response_invalid")
	}
	return out.States, nil
}

// SystemProfiles returns a verified local catalog over the fixed helper UDS.
func (c *Client) SystemProfiles(ctx context.Context) ([]SystemProfile, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime-helper/v1/system-profiles", nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(r)
	if err != nil {
		return nil, fault("helper_transport_unknown")
	}
	defer res.Body.Close()
	var out systemProfilesReply
	if json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&out) != nil {
		return nil, fault("helper_response_invalid")
	}
	if out.Error != nil {
		return nil, out.Error
	}
	if res.StatusCode != http.StatusOK || out.Profiles == nil {
		return nil, fault("helper_response_invalid")
	}
	return out.Profiles, nil
}
