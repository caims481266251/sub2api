package vmruntime

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SystemProfile omits image IDs, host paths and credential details.
type SystemProfile struct {
	ID                string `json:"profile_id"`
	DisplayName       string `json:"display_name"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

var systemProfileNames = []SystemProfile{
	{ID: "centos-stream-9", DisplayName: "CentOS Stream 9"},
	{ID: "ubuntu-24.04", DisplayName: "Ubuntu 24.04 LTS"},
	{ID: "debian-12", DisplayName: "Debian 12"},
	{ID: "archlinux", DisplayName: "Arch Linux"},
	{ID: "fedora-44", DisplayName: "Fedora 44"},
}

// SystemProfiles never inspects account instances or calls a provider.
func (h *Helper) SystemProfiles() []SystemProfile {
	if source, ok := h.driver.(interface{ SystemProfiles() []SystemProfile }); ok {
		return source.SystemProfiles()
	}
	profiles := append([]SystemProfile(nil), systemProfileNames...)
	for i := range profiles {
		profiles[i].UnavailableReason = "runtime_catalog_unavailable"
	}
	return profiles
}

func (d *DockerDriver) SystemProfiles() []SystemProfile {
	d.profileMu.RLock()
	defer d.profileMu.RUnlock()
	if d.profiles != nil {
		return append([]SystemProfile(nil), d.profiles...)
	}
	profiles := append([]SystemProfile(nil), systemProfileNames...)
	for i := range profiles {
		profiles[i].UnavailableReason = "runtime_catalog_unverified"
	}
	return profiles
}

// VerifySystemProfiles runs once at helper startup against operator-owned,
// immutable templates. GET requests only return this verified snapshot.
func (d *DockerDriver) VerifySystemProfiles(ctx context.Context) {
	profiles := append([]SystemProfile(nil), systemProfileNames...)
	hostRaw, hostErr := d.run(ctx, "info", "--format", "{{.Architecture}}")
	host := strings.TrimSpace(string(hostRaw))
	if host == "aarch64" {
		host = "arm64"
	}
	if host == "x86_64" {
		host = "amd64"
	}
	for i := range profiles {
		p := &profiles[i]
		if hostErr != nil || (host != "amd64" && host != "arm64") {
			p.UnavailableReason = "runtime_host_unavailable"
			continue
		}
		if host == "arm64" && p.ID != "ubuntu-24.04" && p.ID != "debian-12" {
			p.UnavailableReason = "architecture_not_supported"
			continue
		}
		template, ok := d.Templates[p.ID]
		if !ok {
			p.UnavailableReason = "profile_not_installed"
			continue
		}
		probe, cancel := context.WithTimeout(ctx, 25*time.Second)
		state := State{Spec: Spec{Platform: "codex", Template: p.ID}}
		err := d.containerPreflight(probe, state)
		if err == nil {
			var raw []byte
			name := "sub2-profile-check-" + uuid.NewString()
			// Verify native Netlink, not just a utility version string. This
			// disposable namespace cannot reach providers or account networks.
			_, err = d.run(probe, "run", "--rm", "--name", name, "--pull=never", "--network=none", "--cap-drop=ALL", "--cap-add=NET_ADMIN", "--security-opt=no-new-privileges", "--entrypoint=/usr/bin/python3", template.Image, "/usr/local/lib/sub2-container/check-runtime.py", "--profile", p.ID, "--network-check")
			if err == nil {
				raw, err = d.run(probe, "run", "--rm", "--name", name, "--pull=never", "--network=none", "--user=10001:10001", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--mount", "type=bind,src="+template.PayloadDir+",dst=/opt/kin-payload,readonly", "--entrypoint=/usr/bin/python3", template.Image, "/usr/local/lib/sub2-container/check-runtime.py", "--profile", p.ID, "--payload")
			}
			// Killing a timed-out Docker CLI does not stop its container. Only
			// remove this uniquely named validation container, never instances.
			cleanup, stopCleanup := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = d.run(cleanup, "rm", "-f", name)
			stopCleanup()
			var result struct {
				Profile string `json:"profile_id"`
				OK      bool   `json:"ok"`
			}
			if err == nil && (json.Unmarshal(raw, &result) != nil || !result.OK || result.Profile != p.ID) {
				err = fault("profile_probe_invalid")
			}
		}
		cancel()
		if err != nil {
			switch errorCode(err) {
			case "image_host_architecture_mismatch":
				p.UnavailableReason = "architecture_not_supported"
			case "pinned_image_missing":
				p.UnavailableReason = "profile_image_missing"
			default:
				p.UnavailableReason = "profile_validation_failed"
			}
			continue
		}
		p.Available = true
	}
	d.profileMu.Lock()
	d.profiles = profiles
	d.profileMu.Unlock()
}
