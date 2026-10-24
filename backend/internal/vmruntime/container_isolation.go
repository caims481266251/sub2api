package vmruntime

import (
	"encoding/hex"
	"net"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

type containerMount struct {
	Type, Source, Destination, Propagation string
	RW                                     bool
}

func validIdentity(i Identity) bool {
	id, err := uuid.Parse(i.UUID)
	if err != nil || id == uuid.Nil || id.String() != i.UUID {
		return false
	}
	machine, err := hex.DecodeString(i.MachineID)
	if err != nil || len(machine) != 16 || strings.ToLower(i.MachineID) != i.MachineID {
		return false
	}
	mac, err := net.ParseMAC(i.MAC)
	if err != nil || len(mac) != 6 || mac.String() != i.MAC || mac[0]&3 != 2 {
		return false
	}
	return i.Hostname == "sub2-"+i.MachineID[:12]
}

func capabilitiesEqual(actual []string, expected ...string) bool {
	if len(actual) != len(expected) {
		return false
	}
	seen := map[string]bool{}
	for _, value := range actual {
		seen[strings.TrimPrefix(strings.ToUpper(value), "CAP_")] = true
	}
	for _, value := range expected {
		if !seen[value] {
			return false
		}
	}
	return len(seen) == len(expected)
}

// Check the daemon's actual configuration, not just the create arguments. This
// runs in the control plane, never in the business request forwarding path.
func checkContainerIsolation(s State, c *containerInspect, dir string, t Template) error {
	h := c.HostConfig
	if s.Network == nil || !validIdentity(s.Identity) || c.Config.Hostname != s.Identity.Hostname {
		return fault("runtime_identity_mismatch")
	}
	if h.Privileged || !h.ReadonlyRootfs || h.PidMode != "" || h.UTSMode != "" ||
		(h.IpcMode != "private" && h.IpcMode != "") || h.NetworkMode != s.Network.NetworkName ||
		h.PublishAllPorts || len(h.PortBindings) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 {
		return fault("runtime_isolation_mismatch")
	}
	if !capabilitiesEqual(h.CapDrop, "ALL") || !capabilitiesEqual(h.CapAdd, "NET_ADMIN", "CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "SETPCAP") {
		return fault("runtime_capabilities_mismatch")
	}
	noNewPrivileges := false
	for _, opt := range h.SecurityOpt {
		if opt == "no-new-privileges" || opt == "no-new-privileges=true" {
			noNewPrivileges = true
			continue
		}
		// Defaults are sufficient. Do not adopt an externally weakened profile.
		return fault("runtime_security_options_mismatch")
	}
	if !noNewPrivileges {
		return fault("runtime_security_options_mismatch")
	}
	expected := map[string]containerMount{
		"/slot/home":        {Source: filepath.Join(dir, "home"), RW: true},
		"/slot/run":         {Source: filepath.Join(dir, "run"), RW: true},
		"/slot/credentials": {Source: filepath.Join(dir, "credentials")},
		"/etc/hosts":        {Source: filepath.Join(dir, "hosts"), RW: true},
		"/etc/machine-id":   {Source: filepath.Join(dir, "run", "machine-id")},
		"/opt/kin-payload":  {Source: t.PayloadDir},
	}
	if len(c.Mounts) != len(expected) {
		return fault("runtime_mount_mismatch")
	}
	for _, mount := range c.Mounts {
		want, ok := expected[mount.Destination]
		if !ok || mount.Type != "bind" || mount.Source != want.Source || mount.RW != want.RW ||
			(mount.Propagation != "rprivate" && mount.Propagation != "") {
			return fault("runtime_mount_mismatch")
		}
		delete(expected, mount.Destination)
	}
	if len(h.Tmpfs) != 2 || h.Tmpfs["/tmp"] != "rw,noexec,nosuid,size=32m" || h.Tmpfs["/run"] != "rw,nosuid,size=16m" {
		return fault("runtime_tmpfs_mismatch")
	}
	return nil
}
