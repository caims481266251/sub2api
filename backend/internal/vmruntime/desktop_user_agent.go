package vmruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Desktop profiles describe simulated clients, not the host's actual OS or TLS
// implementation. The finite catalog is versioned; never rotate existing leases.
// Client versions were checked against openai/codex releases on 2026-10-27.
// macOS versions: support.apple.com/en-us/106337, /109035 and /120283. Format:
// codex-rs/login/src/auth/default_client.rs (originator/version (OS version; arch)).
func desktopUserAgentProfiles(includeLegacy bool) map[string]string {
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
	// These published Sonoma and Sequoia releases extend the compatible pool
	// without changing or inventing any previously assigned template.
	additionalMacVersions := []string{
		"14.8", "14.8.1", "14.8.2", "14.8.3", "14.8.4", "14.8.5",
		"15.5", "15.6", "15.6.1", "15.7", "15.7.1", "15.7.2", "15.7.3", "15.7.4", "15.7.5",
	}
	// Published Ventura versions add capacity without retaining client versions
	// rejected by GPT-6.1 Sol/Terra. Only the explicitly listed 0.159+ clients
	// have ever issued these profiles; keep this history separate from eligibility.
	venturaVersions := []string{
		"13.0", "13.0.1", "13.1", "13.2", "13.2.1", "13.3", "13.3.1", "13.4", "13.4.1",
		"13.5", "13.5.1", "13.5.2", "13.6", "13.6.1", "13.6.2", "13.6.3", "13.6.4", "13.6.5",
		"13.6.6", "13.6.7", "13.6.8", "13.6.9", "13.7", "13.7.1", "13.7.2", "13.7.3",
		"13.7.4", "13.7.5", "13.7.6", "13.7.7", "13.7.8",
	}
	profiles := make(map[string]string)
	for _, client := range clients {
		legacy := legacyDesktopClientVersion(client)
		if legacy && !includeLegacy {
			continue
		}
		add := func(os, version, arch string) {
			id := fmt.Sprintf("desktop-v1/%s/%s/%s/%s", client, os, version, arch)
			profiles[id] = fmt.Sprintf("codex_cli_rs/%s (%s %s; %s)", client, os, version, arch)
		}
		for _, version := range macVersions {
			add("Mac OS", version, "arm64")
			add("Mac OS", version, "x86_64")
		}
		// These profiles were issued from 0.155 onward. Raising the allocation
		// floor must not invalidate their existing manifests.
		switch client {
		case "0.150.0", "0.151.0", "0.152.0", "0.153.0", "0.154.0":
		default:
			for _, version := range additionalMacVersions {
				add("Mac OS", version, "arm64")
				add("Mac OS", version, "x86_64")
			}
		}
		switch client {
		case "0.159.0", "0.160.0", "0.161.0", "0.162.0", "0.162.1", "0.163.0", "0.164.0":
			for _, version := range venturaVersions {
				add("Mac OS", version, "arm64")
				add("Mac OS", version, "x86_64")
			}
		}
		for _, version := range []string{"10.0.19045", "10.0.22631", "10.0.26100"} {
			add("Windows", version, "x86_64")
		}
	}
	return profiles
}

// desktopUserAgents is the allocation pool; historical-only profiles remain
// in desktopUserAgentCatalog and must never be allocated again.
var desktopAllocationProfiles = desktopUserAgentProfiles(false)
var desktopHistoricalProfiles = desktopUserAgentProfiles(true)

func desktopUserAgents() map[string]string { return desktopAllocationProfiles }

func desktopUserAgentCatalog() map[string]string { return desktopHistoricalProfiles }

func legacyDesktopClientVersion(version string) bool {
	switch version {
	// Same-account production probes on 2026-10-27: GPT-6.1 Sol/Terra
	// reject 0.155/0.157/0.158 and complete on 0.159/0.164. The manifest's
	// advertised minimum 0.153 is not the inference admission threshold.
	case "0.150.0", "0.151.0", "0.152.0", "0.153.0", "0.154.0", "0.155.0", "0.156.0", "0.157.0", "0.158.0":
		return true
	default:
		return false
	}
}

func legacyDesktopTemplate(id string) bool {
	parts := strings.Split(id, "/")
	return len(parts) == 5 && parts[0] == "desktop-v1" && legacyDesktopClientVersion(parts[1])
}

// DesktopUserAgentNeedsMigration reports only valid, previously issued profiles
// whose client versions are incompatible with the observed Codex model route.
func DesktopUserAgentNeedsMigration(identity *ClientIdentity) bool {
	if identity == nil || identity.UserAgent == "" || !legacyDesktopTemplate(identity.UserAgentTemplate) {
		return false
	}
	return desktopUserAgentCatalog()[identity.UserAgentTemplate] == identity.UserAgent
}

// One helper owns the runtime root. Serialize scan + durable reservation across
// accounts; only confirmed removed containers release their UA to the pool.
func (h *Helper) ensureDesktopUserAgent(dir string, s *State) error {
	if s.Spec.Platform != "codex" {
		return nil
	}
	h.identityMu.Lock()
	defer h.identityMu.Unlock()
	// Creation writes the first manifest under bindingMu; never scan a partial directory.
	h.bindingMu.Lock()
	defer h.bindingMu.Unlock()
	catalog := desktopUserAgentCatalog()
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
		if old.DeleteRequested && old.Phase == "removed" && old.ContainerID == "" {
			continue
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
	migrate := identity.UserAgent != "" && legacyDesktopTemplate(identity.UserAgentTemplate)
	if identity.UserAgent != "" && !migrate {
		return nil
	}
	// Keep the desktop profile when upgrading an existing identity whenever a
	// compatible UA with the same OS, version and architecture is available.
	candidates := desktopUserAgents()
	ids := sortedDesktopProfileIDs(candidates)
	if migrate {
		old := strings.Split(identity.UserAgentTemplate, "/")
		rank := func(id string) int {
			parts := strings.Split(id, "/")
			if parts[2] == old[2] && parts[3] == old[3] && parts[4] == old[4] {
				return 0
			}
			if parts[2] == old[2] && parts[4] == old[4] {
				return 1
			}
			if parts[2] == old[2] {
				return 2
			}
			return 3
		}
		sort.SliceStable(ids, func(i, j int) bool { return rank(ids[i]) < rank(ids[j]) })
	}
	// Stable ordering makes allocation reproducible; no request-time randomness.
	previous, updatedAt := *identity, s.UpdatedAt
	for _, id := range ids {
		ua := candidates[id]
		if used[ua] {
			continue
		}
		identity.UserAgent, identity.UserAgentTemplate = ua, id
		if err := h.save(dir, s); err != nil {
			*identity, s.UpdatedAt = previous, updatedAt
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
	// Prefer newer compatible clients for new leases and migrations. Within a
	// client version preserve deterministic profile ordering; existing compatible
	// leases do not reach allocation and retain their identities.
	sort.Slice(ids, func(i, j int) bool {
		left, right := strings.Split(ids[i], "/"), strings.Split(ids[j], "/")
		lv, rv := strings.Split(left[1], "."), strings.Split(right[1], ".")
		for component := 0; component < 3; component++ {
			l, _ := strconv.Atoi(lv[component])
			r, _ := strconv.Atoi(rv[component])
			if l != r {
				return l > r
			}
		}
		return ids[i] < ids[j]
	})
	return ids
}
