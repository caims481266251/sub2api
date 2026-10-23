package vmruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

type Template struct {
	Runtime      string            `json:"runtime,omitempty"`
	Acceleration string            `json:"acceleration,omitempty"`
	PayloadDir   string            `json:"payload_dir,omitempty"`
	Image        string            `json:"image"`
	Artifacts    map[string]string `json:"artifacts"` // absolute operator-owned file -> sha256
	// This is a release gate, not a claim inferred from Docker health. Set only
	// after this exact image/guest/payload combination passes the KVM PoC.
	KVMContractVerified          bool `json:"kvm_contract_verified"`
	CredentialProjectionVerified bool `json:"credential_projection_verified"`
}

type CommandRunner interface {
	Run(context.Context, ...string) ([]byte, error)
}
type ExecDocker struct{ Binary string }

func (d ExecDocker) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := d.Binary
	if bin == "" {
		bin = "docker"
	}
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return nil, fault("docker_operation_unknown")
	}
	return out, nil
}

type DockerDriver struct {
	NetworkCIDR      string
	GuestNetworkCIDR string
	ServiceGID       int
	AllowTCG         bool
	Templates        map[string]Template
	Commands         CommandRunner
	HostOS           string
	HostArch         string
}

var immutableImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

func (d *DockerDriver) run(ctx context.Context, args ...string) ([]byte, error) {
	if d.Commands == nil {
		return ExecDocker{}.Run(ctx, args...)
	}
	return d.Commands.Run(ctx, args...)
}
func containerName(s State) string { return "sub2-vm-" + s.InstanceID }

func (d *DockerDriver) template(s State) (Template, error) {
	t, ok := d.Templates[s.Spec.Template]
	if !ok || !(immutableImage.MatchString(t.Image) || (t.Runtime == "upstream-v1" && localImage.MatchString(t.Image))) {
		return t, fault("template_not_allowed")
	}
	if t.Runtime != "" && t.Runtime != "upstream-v1" {
		return t, fault("runtime_not_supported")
	}
	return t, nil
}

func (d *DockerDriver) Preflight(ctx context.Context, s State) error {
	if t, ok := d.Templates[s.Spec.Template]; ok && t.Runtime == "upstream-v1" {
		return d.upstreamPreflight(ctx, s, t)
	}
	osName, arch := d.HostOS, d.HostArch
	if osName == "" {
		osName = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	if osName != "linux" || arch != "amd64" {
		return fault("linux_amd64_required")
	}
	t, err := d.template(s)
	if err != nil {
		return err
	}
	if !t.KVMContractVerified || !t.CredentialProjectionVerified {
		return fault("template_poc_not_verified")
	}
	if len(t.Artifacts) == 0 {
		return fault("artifact_manifest_required")
	}
	for path, digest := range t.Artifacts {
		if !filepath.IsAbs(path) {
			return fault("artifact_path_invalid")
		}
		if err := VerifyArtifact(path, digest); err != nil {
			return err
		}
	}
	// Never pull implicitly. Operator installs the exact digest offline.
	if _, err = d.run(ctx, "image", "inspect", t.Image); err != nil {
		return fault("pinned_image_missing")
	}

	// The fixed image probe starts a tiny KVM machine and issues query-kvm;
	// merely opening /dev/kvm is not accepted as a successful preflight.
	out, err := d.run(ctx, "run", "--rm", "--pull=never", "--network=none", "--device=/dev/kvm", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--entrypoint=/usr/local/lib/sub2-kvm/probe", t.Image)
	if err != nil {
		return fault("kvm_probe_failed")
	}
	var probe struct {
		Enabled bool `json:"enabled"`
		Present bool `json:"present"`
	}
	if json.Unmarshal(out, &probe) != nil || !probe.Enabled || !probe.Present {
		return fault("kvm_not_enabled")
	}
	return nil
}

type containerNetworkEndpoint struct {
	IPAddress, NetworkID string
	IPAMConfig           *struct{ IPv4Address string }
}
type containerInspect struct {
	HostConfig      struct{ NetworkMode string }
	NetworkSettings struct {
		Networks map[string]containerNetworkEndpoint
	}
	ID     string `json:"Id"`
	Config struct {
		Image  string
		Labels map[string]string
	} `json:"Config"`
	State struct {
		Running bool
		Health  *struct{ Status string }
	} `json:"State"`
}

func (d *DockerDriver) inspectContainer(ctx context.Context, name string) (*containerInspect, error) {
	// A failed inspect is not proof of absence. List first, distinguishing a
	// missing object from daemon transport failures before any create/remove.
	out, err := d.run(ctx, "container", "ls", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil, nil
	}
	out, err = d.run(ctx, "inspect", name)
	if err != nil {
		return nil, err
	}
	var rows []containerInspect
	if json.Unmarshal(out, &rows) != nil || len(rows) != 1 {
		return nil, fault("docker_inspect_invalid")
	}
	return &rows[0], nil
}
func owned(c *containerInspect, s State) bool {
	return c != nil && c.Config.Labels["sub2.vm.instance"] == s.InstanceID && c.Config.Labels["sub2.vm.account"] == fmt.Sprint(s.AccountID)
}

func (d *DockerDriver) Ensure(ctx context.Context, s State, dir string) (string, error) {
	t, err := d.template(s)
	if err != nil {
		return "", err
	}
	c, err := d.inspectContainer(ctx, containerName(s))
	if err != nil {
		return "", err
	}
	if c != nil {
		if t.Runtime == "upstream-v1" {
			a, e := d.upstreamAcceleration(t)
			if e != nil {
				return "", e
			}
			if c.Config.Labels["sub2.vm.runtime"] != "upstream-v1" || c.Config.Labels["sub2.vm.acceleration"] != a || (d.ServiceGID > 0 && c.Config.Labels["sub2.vm.service-gid"] != fmt.Sprint(d.ServiceGID)) {
				return "", fault("runtime_policy_conflict")
			}
		}
		if !owned(c, s) || c.Config.Image != t.Image {
			return "", fault("runtime_ownership_conflict")
		}
		if c.State.Running {
			if err := d.checkContainerNetwork(ctx, s, c); err != nil {
				return "", err
			}
		}
		if !c.State.Running {
			if err := d.checkStoppedContainerNetwork(ctx, s, c); err != nil {
				return "", err
			}
			if err := writeInstanceProjection(dir, s, d.ServiceGID); err != nil {
				return "", err
			}
			if _, err = d.run(ctx, "start", c.ID); err != nil {
				return "", err
			}
			started, err := d.inspectContainer(ctx, containerName(s))
			if err != nil {
				return "", err
			}
			if started == nil || !started.State.Running {
				return "", fault("runtime_start_unconfirmed")
			}
			if err := d.checkContainerNetwork(ctx, s, started); err != nil {
				if stopErr := d.Stop(ctx, s); stopErr != nil {
					return "", fault("runtime_network_stop_unconfirmed")
				}
				return "", err
			}
		}
		return c.ID, nil
	}
	for _, sub := range []string{"home", "run", "kvm", "credentials"} {
		if err := privateDir(filepath.Join(dir, sub)); err != nil {
			return "", err
		}
	}
	if err := writeInstanceProjection(dir, s, d.ServiceGID); err != nil {
		return "", err
	}
	if t.Runtime == "upstream-v1" {
		return d.upstreamCreate(ctx, s, dir, t)
	}
	if _, err := d.ensureNetwork(ctx, s); err != nil {
		return "", err
	}
	network := "bridge"
	if s.Network != nil {
		network = s.Network.NetworkName
	}
	args := []string{"run", "-d", "--pull=never", "--name", containerName(s), "--network=" + network, "--sysctl=net.ipv6.conf.all.disable_ipv6=1", "--device=/dev/kvm", "--cap-drop=ALL", "--cap-add=NET_ADMIN", "--cap-add=CHOWN", "--cap-add=SETUID", "--cap-add=SETGID", "--cap-add=DAC_OVERRIDE", "--security-opt=no-new-privileges", "--stop-timeout=40", "--pids-limit=256", "--memory", fmt.Sprintf("%dm", s.Spec.MemoryMiB+512), "--label", "sub2.vm.instance=" + s.InstanceID, "--label", fmt.Sprintf("sub2.vm.account=%d", s.AccountID)}
	if s.Network != nil {
		args = append(args, "--ip="+s.Network.Address)
	}
	for _, sub := range []string{"home", "run", "kvm", "credentials"} {
		mode := ""
		if sub == "credentials" {
			mode = ":ro"
		}
		args = append(args, "--mount", "type=bind,src="+filepath.Join(dir, sub)+",dst=/slot/"+sub+strings.ReplaceAll(mode, ":ro", ",readonly"))
	}
	args = append(args, t.Image)
	out, err := d.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (d *DockerDriver) Inspect(ctx context.Context, s State, dir string) (Observation, error) {
	c, err := d.inspectContainer(ctx, containerName(s))
	if err != nil {
		return Observation{}, err
	}
	if c == nil {
		return Observation{}, nil
	}
	if !owned(c, s) {
		return Observation{}, fault("runtime_ownership_conflict")
	}
	if !c.State.Running {
		return Observation{}, nil
	}
	t, err := d.template(s)
	if err != nil {
		return Observation{}, err
	}
	if c.Config.Image != t.Image || (t.Runtime != "upstream-v1" && (!t.KVMContractVerified || !t.CredentialProjectionVerified)) {
		return Observation{}, fault("runtime_template_unverified")
	}
	if err := d.checkContainerNetwork(ctx, s, c); err != nil {
		return Observation{}, err
	}
	script := "/usr/local/lib/sub2-kvm/observe"
	if t.Runtime == "upstream-v1" {
		script = "/usr/local/lib/sub2-vm/observe"
	}
	out, err := d.run(ctx, "exec", containerName(s), script)
	if err != nil {
		return Observation{}, err
	}
	var wire struct {
		Capacity           int    `json:"capacity"`
		ActualAccelerator  string `json:"actual_accelerator"`
		Generation         uint64 `json:"generation"`
		KVMEnabled         bool   `json:"kvm_enabled"`
		GuestBootID        string `json:"guest_boot_id"`
		CredentialRevision string `json:"credential_revision"`
		ProxyRevision      string `json:"proxy_revision"`
		KernelReady        bool   `json:"kernel_ready"`
	}
	if json.Unmarshal(out, &wire) != nil {
		return Observation{}, fault("guest_observation_invalid")
	}
	if wire.Generation != s.Generation {
		return Observation{}, fault("guest_generation_mismatch")
	}
	if t.Runtime == "upstream-v1" {
		a, e := d.upstreamAcceleration(t)
		if e != nil {
			return Observation{}, e
		}
		if c.Config.Labels["sub2.vm.runtime"] != "upstream-v1" || c.Config.Labels["sub2.vm.acceleration"] != a || (d.ServiceGID > 0 && c.Config.Labels["sub2.vm.service-gid"] != fmt.Sprint(d.ServiceGID)) {
			return Observation{}, fault("runtime_policy_conflict")
		}
		if wire.Capacity < 0 || wire.ActualAccelerator != a || (a == "kvm" && !wire.KVMEnabled) || (a == "tcg" && wire.KVMEnabled) {
			return Observation{}, fault("runtime_observation_policy_mismatch")
		}
	}
	return Observation{ActualAccelerator: wire.ActualAccelerator, Running: true, KVMEnabled: wire.KVMEnabled, GuestBootID: wire.GuestBootID, CredentialRevision: wire.CredentialRevision, ProxyRevision: wire.ProxyRevision, KernelReady: wire.KernelReady}, nil
}
func (d *DockerDriver) Apply(ctx context.Context, s State, dir string) (Observation, error) {
	// The fixed runner reads this non-secret desired version and emits a
	// guest-derived receipt after restarting its kernel, not a host-only ack.
	if err := writeInstanceProjection(dir, s, d.ServiceGID); err != nil {
		return Observation{}, err
	}
	if _, err := d.Ensure(ctx, s, dir); err != nil {
		return Observation{}, err
	}
	script := "/usr/local/lib/sub2-kvm/apply"
	if t, err := d.template(s); err != nil {
		return Observation{}, err
	} else if t.Runtime == "upstream-v1" {
		script = "/usr/local/lib/sub2-vm/apply"
	}
	if _, err := d.run(ctx, "exec", containerName(s), script); err != nil {
		return Observation{}, err
	}
	return d.Inspect(ctx, s, dir)
}
func (d *DockerDriver) Stop(ctx context.Context, s State) error {
	c, err := d.inspectContainer(ctx, containerName(s))
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	if !owned(c, s) {
		return fault("runtime_ownership_conflict")
	}
	if c.State.Running {
		if _, err = d.run(ctx, "stop", "--time=40", c.ID); err != nil {
			return err
		}
	}
	c, err = d.inspectContainer(ctx, containerName(s))
	if err != nil {
		return err
	}
	if c != nil && c.State.Running {
		return fault("runtime_still_running")
	}
	return nil
}
func (d *DockerDriver) Remove(ctx context.Context, s State) error {
	c, err := d.inspectContainer(ctx, containerName(s))
	if err != nil {
		return err
	}
	if c == nil {
		return d.removeNetwork(ctx, s)
	}
	if !owned(c, s) {
		return fault("runtime_ownership_conflict")
	}
	if c.State.Running {
		return fault("runtime_must_be_stopped")
	}
	if _, err = d.run(ctx, "rm", c.ID); err != nil {
		return err
	}
	c, err = d.inspectContainer(ctx, containerName(s))
	if err != nil {
		return err
	}
	if c != nil {
		return fault("runtime_removal_unconfirmed")
	}
	return d.removeNetwork(ctx, s)
}
