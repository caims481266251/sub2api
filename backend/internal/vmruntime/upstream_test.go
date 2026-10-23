package vmruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type upstreamCommands struct {
	calls                         [][]string
	serverArch, imageArch, actual string
	enabled                       bool
}

func (f *upstreamCommands) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	switch args[0] {
	case "version":
		return []byte(fmt.Sprintf(`{"Os":"linux","Arch":%q}`, f.serverArch)), nil
	case "image":
		return []byte(fmt.Sprintf(`{"Os":"linux","Architecture":%q}`, f.imageArch)), nil
	case "run":
		if strings.Contains(strings.Join(args, " "), "/probe") {
			return []byte(fmt.Sprintf(`{"actual_accelerator":%q,"enabled":%t,"present":true}`, f.actual, f.enabled)), nil
		}
		return []byte("fixture-container"), nil
	}
	return nil, fault("unexpected_test_command")
}
func upstreamFixture(t *testing.T) (*DockerDriver, State, *upstreamCommands) {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "kin-kernel")
	data := []byte("fixture-payload")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	c := &upstreamCommands{serverArch: "arm64", imageArch: "arm64", actual: "tcg"}
	d := &DockerDriver{AllowTCG: true, Commands: c, Templates: map[string]Template{"fixed": {Runtime: "upstream-v1", Acceleration: "tcg", Image: "sha256:" + strings.Repeat("a", 64), PayloadDir: root, Artifacts: map[string]string{file: hex.EncodeToString(sum[:])}}}}
	return d, State{AccountID: 42, InstanceID: "fixture", Spec: Spec{Template: "fixed", MemoryMiB: 1024}}, c
}
func TestUpstreamPreflightPolicyAndPlatform(t *testing.T) {
	for _, name := range []string{"native_arm_tcg", "tcg_denied", "image_mismatch", "kvm_arm_denied", "probe_mismatch", "unmanifested"} {
		t.Run(name, func(t *testing.T) {
			d, s, c := upstreamFixture(t)
			want := ""
			switch name {
			case "tcg_denied":
				d.AllowTCG = false
				want = "tcg_not_allowed"
			case "image_mismatch":
				c.imageArch = "amd64"
				want = "image_platform_mismatch"
			case "kvm_arm_denied":
				v := d.Templates["fixed"]
				v.Acceleration = ""
				d.Templates["fixed"] = v
				want = "kvm_x86_host_required"
			case "probe_mismatch":
				c.enabled = true
				want = "accelerator_probe_mismatch"
			case "unmanifested":
				if err := os.WriteFile(filepath.Join(d.Templates["fixed"].PayloadDir, "extra"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				want = "unmanifested_payload"
			}
			err := d.Preflight(context.Background(), s)
			if want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if errorCode(err) != want {
				t.Fatalf("got %v want %s", err, want)
			}
		})
	}
}
func TestUpstreamCreatePinsPolicyAndMounts(t *testing.T) {
	d, s, c := upstreamFixture(t)
	_, err := d.upstreamCreate(context.Background(), s, t.TempDir(), d.Templates["fixed"])
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(c.calls[len(c.calls)-1], " ")
	for _, want := range []string{"SUB2_VM_ACCEL=tcg", "KIN_KVM_CPU_MODEL=max", "KIN_KVM_UID=10001", "sub2.vm.acceleration=tcg", "dst=/slot/credentials,readonly", "dst=/opt/kin-guest,readonly", "--entrypoint=/usr/local/lib/sub2-vm/entry"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(args, "--device=/dev/kvm") {
		t.Fatal("TCG mounted KVM")
	}
}
func TestUpstreamTCGRequiresBothPolicyLayers(t *testing.T) {
	d, s, _ := upstreamFixture(t)
	if !d.AllowsTCG(s) {
		t.Fatal("explicit test policy denied")
	}
	d.AllowTCG = false
	if d.AllowsTCG(s) {
		t.Fatal("global gate bypassed")
	}
	d.AllowTCG = true
	v := d.Templates["fixed"]
	v.Acceleration = ""
	d.Templates["fixed"] = v
	if d.AllowsTCG(s) {
		t.Fatal("default KVM became TCG")
	}
}

type tcgObservationDriver struct {
	fakeDriver
	allow   bool
	actual  string
	enabled bool
}

func (f *tcgObservationDriver) AllowsTCG(State) bool { return f.allow }
func (f *tcgObservationDriver) Apply(_ context.Context, s State, _ string) (Observation, error) {
	return Observation{Running: true, ActualAccelerator: f.actual, KVMEnabled: f.enabled, KernelReady: true, GuestBootID: "boot-tcg", CredentialRevision: s.DesiredCredentialRevision, ProxyRevision: s.Spec.ProxyRevision}, nil
}
func TestHelperActualAcceleratorReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, actual          string
		allow, enabled, ready bool
	}{
		{"denied_tcg", "tcg", false, false, false}, {"explicit_tcg", "tcg", true, false, true}, {"fake_kvm", "tcg", false, true, false}, {"tcg_claims_kvm", "tcg", true, true, false}, {"real_kvm", "kvm", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := openTest(t, &tcgObservationDriver{allow: tc.allow, actual: tc.actual, enabled: tc.enabled})
			s, err := h.Do(context.Background(), prepareRequest())
			if err != nil {
				t.Fatal(err)
			}
			if s.Ready != tc.ready || s.ActualAccelerator != tc.actual {
				t.Fatalf("incorrect readiness %t accelerator %s", s.Ready, s.ActualAccelerator)
			}
		})
	}
}

func TestInstanceProjectionReplacementRemainsPrivate(t *testing.T) {
	dir := t.TempDir()
	for _, generation := range []uint64{1, 2} {
		if err := writeInstanceProjection(dir, State{Generation: generation}); err != nil {
			t.Fatal(err)
		}
		for _, item := range []struct {
			path string
			mode os.FileMode
		}{{filepath.Join(dir, "run"), 0700}, {filepath.Join(dir, "run", "instance.json"), 0600}} {
			st, err := os.Stat(item.path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != item.mode {
				t.Fatalf("unexpected projection permissions %o", st.Mode().Perm())
			}
		}
		b, err := os.ReadFile(filepath.Join(dir, "run", "instance.json"))
		if err != nil {
			t.Fatal(err)
		}
		var state State
		if err = json.Unmarshal(b, &state); err != nil {
			t.Fatal(err)
		}
		if state.Generation != generation {
			t.Fatal("stale projection")
		}
	}
}
