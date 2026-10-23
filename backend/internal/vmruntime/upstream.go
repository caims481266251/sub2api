package vmruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var localImage = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func (d *DockerDriver) AllowsTCG(s State) bool {
	t, err := d.template(s)
	return err == nil && t.Runtime == "upstream-v1" && t.Acceleration == "tcg" && d.AllowTCG
}
func (d *DockerDriver) upstreamAcceleration(t Template) (string, error) {
	a := t.Acceleration
	if a == "" {
		a = "kvm"
	}
	if a != "kvm" && a != "tcg" {
		return "", fault("accelerator_invalid")
	}
	if a == "tcg" && !d.AllowTCG {
		return "", fault("tcg_not_allowed")
	}
	return a, nil
}
func (d *DockerDriver) upstreamPreflight(ctx context.Context, s State, t Template) error {
	if _, err := d.template(s); err != nil {
		return err
	}
	accel, err := d.upstreamAcceleration(t)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(t.PayloadDir) {
		return fault("payload_path_invalid")
	}
	info, err := os.Lstat(t.PayloadDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fault("payload_path_invalid")
	}
	// Every payload file must be pinned; reject symlinks and unmanifested additions.
	count := 0
	err = filepath.WalkDir(t.PayloadDir, func(path string, e os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fault("payload_unreadable")
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fault("payload_symlink_forbidden")
		}
		if e.IsDir() {
			return nil
		}
		if !e.Type().IsRegular() {
			return fault("payload_file_invalid")
		}
		digest, ok := t.Artifacts[path]
		if !ok {
			return fault("unmanifested_payload")
		}
		count++
		return VerifyArtifact(path, digest)
	})
	if err != nil {
		return err
	}
	if count == 0 {
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
	out, err := d.run(ctx, "version", "--format", "{{json .Server}}")
	if err != nil {
		return err
	}
	var server struct {
		Os   string `json:"Os"`
		Arch string `json:"Arch"`
	}
	if json.Unmarshal(out, &server) != nil || server.Os != "linux" || (server.Arch != "amd64" && server.Arch != "arm64") {
		return fault("docker_platform_unsupported")
	}
	out, err = d.run(ctx, "image", "inspect", "--format", "{{json .}}", t.Image)
	if err != nil {
		return fault("pinned_image_missing")
	}
	var im struct {
		Architecture string
		Os           string
	}
	if json.Unmarshal(out, &im) != nil || im.Os != server.Os || im.Architecture != server.Arch {
		return fault("image_platform_mismatch")
	}
	if accel == "kvm" && server.Arch != "amd64" {
		return fault("kvm_x86_host_required")
	}
	args := []string{"run", "--rm", "--pull=never", "--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges"}
	if accel == "kvm" {
		args = append(args, "--device=/dev/kvm")
	}
	args = append(args, "--entrypoint=/usr/local/lib/sub2-vm/probe", t.Image, "--accel", accel)
	out, err = d.run(ctx, args...)
	if err != nil {
		return fault("accelerator_probe_failed")
	}
	var p struct {
		Actual  string `json:"actual_accelerator"`
		Enabled bool   `json:"enabled"`
		Present bool   `json:"present"`
	}
	if json.Unmarshal(out, &p) != nil || p.Actual != accel || (accel == "kvm" && (!p.Enabled || !p.Present)) || (accel == "tcg" && p.Enabled) {
		return fault("accelerator_probe_mismatch")
	}
	return nil
}
func (d *DockerDriver) upstreamCreate(ctx context.Context, s State, dir string, t Template) (string, error) {
	accel, err := d.upstreamAcceleration(t)
	if err != nil {
		return "", err
	}
	createdNetwork, err := d.ensureNetwork(ctx, s)
	if err != nil {
		return "", err
	}
	network := "bridge"
	if s.Network != nil {
		network = s.Network.NetworkName
	}
	args := []string{"run", "-d", "--pull=never", "--name", containerName(s), "--network=" + network, "--sysctl=net.ipv6.conf.all.disable_ipv6=1", "--cap-drop=ALL", "--cap-add=NET_ADMIN", "--cap-add=CHOWN", "--cap-add=SETUID", "--cap-add=SETGID", "--cap-add=DAC_OVERRIDE", "--security-opt=no-new-privileges", "--stop-timeout=40", "--pids-limit=256", "--memory", fmt.Sprintf("%dm", s.Spec.MemoryMiB+512), "--label", "sub2.vm.instance=" + s.InstanceID, "--label", fmt.Sprintf("sub2.vm.account=%d", s.AccountID)}
	if s.Network != nil {
		args = append(args, "--ip="+s.Network.Address)
	}
	args = append(args, "--label", "sub2.vm.runtime=upstream-v1", "--label", "sub2.vm.acceleration="+accel, "--label", fmt.Sprintf("sub2.vm.service-gid=%d", d.ServiceGID))
	if accel == "kvm" {
		args = append(args, "--device=/dev/kvm")
	}
	env := map[string]string{"SUB2_VM_ACCEL": accel, "KIN_KVM_ACCEL": accel, "KIN_VM_ID": s.InstanceID, "KIN_KVM_UID": "10001", "KIN_KVM_GID": "10001", "KIN_KVM_MEMORY_MB": fmt.Sprint(s.Spec.MemoryMiB), "KIN_KVM_VCPUS": fmt.Sprint(s.Spec.CPU), "KIN_KVM_DISK_GB": fmt.Sprint(s.Spec.DiskGiB), "KIN_KVM_CPU_MODEL": "host", "KIN_KVM_MAC": s.Identity.MAC, "KIN_KVM_DISK_SERIAL": s.Identity.DiskSerial, "KIN_KVM_HOSTNAME": s.Identity.Hostname, "KIN_KVM_MACHINE_ID": s.Identity.MachineID, "KIN_KVM_SMBIOS_UUID": s.Identity.UUID, "KIN_KVM_SMBIOS_SERIAL": s.Identity.Serial, "KIN_KVM_SMBIOS_MANUFACTURER": "Sub2API", "KIN_KVM_SMBIOS_PRODUCT": "AccountVM", "KIN_KVM_SMBIOS_VERSION": "1", "KIN_KVM_SMBIOS_FAMILY": "Sub2API"}
	if accel == "tcg" {
		env["KIN_KVM_CPU_MODEL"] = "max"
	}
	if d.ServiceGID > 0 {
		env["SUB2_VM_SERVICE_GID"] = fmt.Sprint(d.ServiceGID)
	}
	for k, v := range env {
		args = append(args, "--env", k+"="+v)
	}
	for _, sub := range []string{"home", "run", "kvm", "credentials"} {
		m := "type=bind,src=" + filepath.Join(dir, sub) + ",dst=/slot/" + sub
		if sub == "credentials" {
			m += ",readonly"
		}
		args = append(args, "--mount", m)
	}
	args = append(args, "--mount", "type=bind,src="+t.PayloadDir+",dst=/opt/kin-guest,readonly", "--entrypoint=/usr/local/lib/sub2-vm/entry", t.Image)
	out, err := d.run(ctx, args...)
	if err != nil {
		// A failed command may have created a container. Reclaim only when a
		// subsequent successful inventory proves absence; retain lease on doubt.
		if createdNetwork {
			c, inspectErr := d.inspectContainer(ctx, containerName(s))
			if inspectErr == nil && c == nil {
				if cleanupErr := d.removeNetwork(ctx, s); cleanupErr != nil {
					return "", cleanupErr
				}
			}
		}
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fault("docker_create_unknown")
	}
	return id, nil
}

// writeInstanceProjection keeps the shared metadata private yet readable by
// the fixed QEMU uid across atomic replacements and stopped-container restarts.
func writeInstanceProjection(dir string, s State, serviceGID ...int) error {
	run := filepath.Join(dir, "run")
	if err := privateDir(run); err != nil {
		return err
	}
	uid, gid := -1, -1
	if os.Geteuid() == 0 {
		uid, gid = 10001, 10001
		if err := os.Chown(run, uid, gid); err != nil {
			return err
		}
	}
	if len(serviceGID) > 0 && serviceGID[0] > 0 {
		if err := serviceDirectory(run, 10001, serviceGID[0]); err != nil {
			return err
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicBytesOwned(filepath.Join(run, "instance.json"), b, uid, gid)
}
