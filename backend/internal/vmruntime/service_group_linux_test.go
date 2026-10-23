package vmruntime

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestServiceGroupNonRootIsolation(t *testing.T) {
	if os.Getenv("SUB2_GROUP_TEST_CHILD") == "1" {
		socket, secret := os.Getenv("SUB2_GROUP_TEST_SOCKET"), os.Getenv("SUB2_GROUP_TEST_SECRET")
		c, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		if _, err := os.ReadFile(secret); err == nil {
			t.Fatal("non-root service read private manifest")
		}
		if _, err := os.ReadFile(filepath.Join(filepath.Dir(secret), "credentials", "credential.json")); err == nil {
			t.Fatal("non-root service read credential")
		}
		if _, err := os.ReadDir(filepath.Dir(socket)); err == nil {
			t.Fatal("non-root service listed private run")
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires Linux root to verify actual non-root peer")
	}
	root, err := os.MkdirTemp("/tmp", "sub2-group-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	h, err := OpenHelper(root, &fakeDriver{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err = h.ConfigureServiceGroup(23456); err != nil {
		t.Fatal(err)
	}
	s, err := h.Do(t.Context(), prepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, s.InstanceID)
	run := filepath.Join(dir, "run")
	if err = writeInstanceProjection(dir, *s, 23456); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(run, "kernel.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = os.Chown(socket, 10001, 23456); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(socket, 0660); err != nil {
		t.Fatal(err)
	}
	// Copy test binary to an accessible location; go test's cache paths are private.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "peer-test")
	if err = os.WriteFile(child, b, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(child, "-test.run=^TestServiceGroupNonRootIsolation$")
	cmd.Env = append(os.Environ(), "SUB2_GROUP_TEST_CHILD=1", "SUB2_GROUP_TEST_SOCKET="+socket, "SUB2_GROUP_TEST_SECRET="+filepath.Join(dir, "manifest.json"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 23457, Gid: 23456}}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("non-root isolation failed: %v %s", err, output)
	}
}
