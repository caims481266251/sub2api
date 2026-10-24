package vmruntime

import (
	"path/filepath"
	"strings"
	"testing"
)

func isolationFixture(t *testing.T, dir string) (State, containerInspect, Template) {
	t.Helper()
	identity, err := newIdentity()
	if err != nil {
		t.Fatal(err)
	}
	s := State{Identity: identity, Network: &NetworkLease{NetworkName: "account-network"}}
	template := Template{PayloadDir: filepath.Join(dir, "payload")}
	c := containerInspect{}
	c.Config.Hostname = identity.Hostname
	c.HostConfig.NetworkMode = s.Network.NetworkName
	c.HostConfig.ReadonlyRootfs = true
	c.HostConfig.CapDrop = []string{"ALL"}
	c.HostConfig.CapAdd = []string{"NET_ADMIN", "CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "SETPCAP"}
	c.HostConfig.SecurityOpt = []string{"no-new-privileges"}
	c.HostConfig.Tmpfs = map[string]string{"/tmp": "rw,noexec,nosuid,size=32m", "/run": "rw,nosuid,size=16m"}
	for _, sub := range []string{"home", "run", "credentials"} {
		c.Mounts = append(c.Mounts, containerMount{Type: "bind", Source: filepath.Join(dir, sub), Destination: "/slot/" + sub, RW: sub != "credentials", Propagation: "rprivate"})
	}
	c.Mounts = append(c.Mounts,
		containerMount{Type: "bind", Source: filepath.Join(dir, "hosts"), Destination: "/etc/hosts", RW: true},
		containerMount{Type: "bind", Source: filepath.Join(dir, "run", "machine-id"), Destination: "/etc/machine-id"},
		containerMount{Type: "bind", Source: template.PayloadDir, Destination: "/opt/kin-payload"})
	return s, c, template
}

func TestContainerIsolationRejectsCrossAccountAndWeakenedConfiguration(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]func(*containerInspect){
		"foreign_home":         func(c *containerInspect) { c.Mounts[0].Source = filepath.Join(dir, "other-account", "home") },
		"writable_credentials": func(c *containerInspect) { c.Mounts[2].RW = true },
		"docker_socket": func(c *containerInspect) {
			c.Mounts = append(c.Mounts, containerMount{Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", Type: "bind", RW: true})
		},
		"shared_mount_propagation": func(c *containerInspect) { c.Mounts[0].Propagation = "rshared" },
		"host_pid":                 func(c *containerInspect) { c.HostConfig.PidMode = "host" },
		"shared_ipc":               func(c *containerInspect) { c.HostConfig.IpcMode = "container:other" },
		"host_uts":                 func(c *containerInspect) { c.HostConfig.UTSMode = "host" },
		"privileged":               func(c *containerInspect) { c.HostConfig.Privileged = true },
		"writable_root":            func(c *containerInspect) { c.HostConfig.ReadonlyRootfs = false },
		"cap_sys_admin":            func(c *containerInspect) { c.HostConfig.CapAdd = append(c.HostConfig.CapAdd, "SYS_ADMIN") },
		"seccomp_disabled": func(c *containerInspect) {
			c.HostConfig.SecurityOpt = append(c.HostConfig.SecurityOpt, "seccomp=unconfined")
		},
		"new_privileges":         func(c *containerInspect) { c.HostConfig.SecurityOpt = nil },
		"wrong_hostname":         func(c *containerInspect) { c.Config.Hostname = "other" },
		"tmpfs_over_credentials": func(c *containerInspect) { c.HostConfig.Tmpfs["/slot/credentials"] = "rw" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, c, template := isolationFixture(t, dir)
			if err := checkContainerIsolation(s, &c, dir, template); err != nil {
				t.Fatal(err)
			}
			mutate(&c)
			if err := checkContainerIsolation(s, &c, dir, template); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestIdentityValidation(t *testing.T) {
	valid, err := newIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !validIdentity(valid) {
		t.Fatal("generated identity rejected")
	}
	for _, field := range []string{"uuid", "machine", "mac", "host"} {
		bad := valid
		switch field {
		case "uuid":
			bad.UUID = "invalid"
		case "machine":
			bad.MachineID = strings.Repeat("g", 32)
		case "mac":
			bad.MAC = "00:11:22:33:44:55"
		case "host":
			bad.Hostname = "other"
		}
		if validIdentity(bad) {
			t.Fatal("invalid identity accepted", field)
		}
	}
}
