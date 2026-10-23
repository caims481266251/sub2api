//go:build !windows

package vmruntime

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPrepareCoordinatorLockFreshAndRepeatedStartup(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "helper.sock")
	if err := PrepareCoordinatorLock(socket, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	path := socket + ".coordinator.lock"
	before, err := os.Stat(path)
	if err != nil || before.Mode().Perm() != 0600 {
		t.Fatalf("fresh lock permissions: %v", err)
	}
	owner, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := syscall.Flock(int(owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := PrepareCoordinatorLock(socket, os.Getuid()); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil || !os.SameFile(before, after) {
			t.Fatal("helper restart replaced coordinator lock")
		}
		other, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		other.Close()
		if err == nil {
			t.Fatal("helper restart allowed a second coordinator")
		}
	}
}

func TestPrepareCoordinatorLockRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			socket := filepath.Join(dir, "helper.sock")
			path, target := socket+".coordinator.lock", filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("preserve"), 0644); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "hardlink":
				err = os.Link(target, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "directory":
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if PrepareCoordinatorLock(socket, os.Getuid()) == nil {
				t.Fatal("unsafe lock accepted")
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "preserve" {
				t.Fatal("target was modified")
			}
		})
	}
}
