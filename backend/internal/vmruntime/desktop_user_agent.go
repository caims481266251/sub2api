package vmruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Desktop profiles describe simulated clients, not the host's actual OS or TLS
// implementation. The finite catalog is versioned; never rotate existing leases.
// Client versions were checked against openai/codex releases on 2026-10-27.
// macOS versions: support.apple.com/en-us/109035 and /120283. Format:
// codex-rs/login/src/auth/default_client.rs (originator/version (OS version; arch)).
func desktopUserAgents() map[string]string {
	// Explicit release lists, not generated/fictitious patch numbers. Keep older
	// entries forever: existing manifests reference these immutable template IDs.
	clients := []string{
		"0.150.0", "0.151.0", "0.152.0", "0.153.0", "0.154.0", "0.155.0",
		"0.156.0", "0.157.0", "0.158.0", "0.159.0", "0.160.0", "0.161.0",
		"0.162.0", "0.162.1", "0.163.0", "0.164.0",
	}
	macVersions := []string{
		"14.0", "14.1", "14.1.1", "14.1.2", "14.2", "14.2.1", "14.3", "14.3.1",
		"14.4", "14.4.1", "14.5", "14.6", "14.6.1", "14.7", "14.7.1", "14.7.2",
		"14.7.3", "14.7.4", "14.7.5", "14.7.6", "14.7.7", "14.7.8",
		"15.0", "15.0.1", "15.1", "15.1.1", "15.2", "15.3", "15.3.1", "15.3.2", "15.4", "15.4.1",
	}
	profiles := make(map[string]string, 1072)
	for _, client := range clients {
		add := func(os, version, arch string) {
			id := fmt.Sprintf("desktop-v1/%s/%s/%s/%s", client, os, version, arch)
			profiles[id] = fmt.Sprintf("codex_cli_rs/%s (%s %s; %s)", client, os, version, arch)
		}
		for _, version := range macVersions {
			add("Mac OS", version, "arm64")
			add("Mac OS", version, "x86_64")
		}
		for _, version := range []string{"10.0.19045", "10.0.22631", "10.0.26100"} {
			add("Windows", version, "x86_64")
		}
	}
	return profiles
}

// One helper owns the runtime root. Serialize scan + durable reservation across
// accounts; retain tombstone reservations so delete/recreate cannot recycle UA.
func (h *Helper) ensureDesktopUserAgent(dir string, s *State) error {
	if s.Spec.Platform != "codex" {
		return nil
	}
	h.identityMu.Lock()
	defer h.identityMu.Unlock()
	// Creation writes the first manifest under bindingMu; never scan a partial directory.
	h.bindingMu.Lock()
	defer h.bindingMu.Unlock()
	catalog := desktopUserAgents()
	identity := s.ClientIdentity
	if (identity.UserAgent == "") != (identity.UserAgentTemplate == "") {
		return fault("client_user_agent_invalid")
	}
	if identity.UserAgent != "" && catalog[identity.UserAgentTemplate] != identity.UserAgent {
		return fault("client_user_agent_invalid")
	}
	entries, err := os.ReadDir(h.root)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == s.InstanceID {
			continue
		}
		old, err := h.read(filepath.Join(h.root, entry.Name()))
		if err != nil {
			return fault("manifest_inventory_unreadable")
		}
		if old.ClientIdentity == nil || old.ClientIdentity.UserAgent == "" {
			continue
		}
		ua := old.ClientIdentity.UserAgent
		if used[ua] || ua == identity.UserAgent {
			return fault("client_user_agent_conflict")
		}
		used[ua] = true
	}
	if identity.UserAgent != "" {
		return nil
	}
	// Stable ordering makes allocation reproducible; no request-time randomness.
	for _, id := range sortedDesktopProfileIDs(catalog) {
		ua := catalog[id]
		if used[ua] {
			continue
		}
		identity.UserAgent, identity.UserAgentTemplate = ua, id
		if err := h.save(dir, s); err != nil {
			identity.UserAgent, identity.UserAgentTemplate = "", ""
			return err
		}
		return nil
	}
	return fault("client_user_agent_pool_exhausted")
}

func sortedDesktopProfileIDs(catalog map[string]string) []string {
	ids := make([]string, 0, len(catalog))
	for id := range catalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
