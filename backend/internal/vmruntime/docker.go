package vmruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

type Template struct {
	Runtime    string            `json:"runtime"`
	PayloadDir string            `json:"payload_dir"`
	Image      string            `json:"image"`
	Artifacts  map[string]string `json:"artifacts"`
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
	cmd := exec.CommandContext(ctx, bin, args...)
	// A management-image default must not force emulation of native nftables.
	// The fixed template is verified against the Docker server architecture.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "DOCKER_DEFAULT_PLATFORM=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fault("docker_operation_unknown")
	}
	return out, nil
}

type DockerDriver struct {
	NetworkCIDR string
	ServiceGID  int
	Templates   map[string]Template
	Commands    CommandRunner
}

var immutableImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var localImage = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func (d *DockerDriver) run(ctx context.Context, args ...string) ([]byte, error) {
	if d.Commands == nil {
		return ExecDocker{}.Run(ctx, args...)
	}
	return d.Commands.Run(ctx, args...)
}
func containerName(s State) string { return "sub2-docker-" + s.InstanceID }
func (d *DockerDriver) template(s State) (Template, error) {
	t, ok := d.Templates[s.Spec.Template]
	if !ok || (!immutableImage.MatchString(t.Image) && !localImage.MatchString(t.Image)) {
		return t, fault("template_not_allowed")
	}
	if t.Runtime != "container-v1" {
		return t, fault("runtime_not_supported")
	}
	return t, nil
}
func (d *DockerDriver) Preflight(ctx context.Context, s State) error {
	return d.containerPreflight(ctx, s)
}
func (d *DockerDriver) Ensure(ctx context.Context, s State, dir string) (string, error) {
	return d.containerEnsure(ctx, s, dir)
}
func (d *DockerDriver) Inspect(ctx context.Context, s State, dir string) (Observation, error) {
	return d.containerInspect(ctx, s, dir)
}
func (d *DockerDriver) Apply(ctx context.Context, s State, dir string) (Observation, error) {
	return d.containerApply(ctx, s, dir)
}

type containerNetworkEndpoint struct {
	IPAddress, NetworkID, MacAddress string
	IPAMConfig                       *struct{ IPv4Address string }
}
type containerInspect struct {
	HostConfig struct {
		NetworkMode, PidMode, IpcMode, UTSMode      string
		Privileged, ReadonlyRootfs, PublishAllPorts bool
		CapAdd, CapDrop, SecurityOpt                []string
		Tmpfs                                       map[string]string
		PortBindings                                map[string]json.RawMessage
		Devices                                     []json.RawMessage
		DeviceRequests                              []json.RawMessage
	}
	Mounts          []containerMount
	NetworkSettings struct {
		Networks map[string]containerNetworkEndpoint
	}
	ID     string `json:"Id"`
	Config struct {
		Image    string
		Hostname string
		Labels   map[string]string
	} `json:"Config"`
	State struct {
		Running   bool
		StartedAt string
		Health    *struct{ Status string }
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
