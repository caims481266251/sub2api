//go:build !windows

package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestVMKernelStartupGuard(t *testing.T) {
	cfg := &config.Config{}
	cfg.VMKernel.HelperSocket = filepath.Join(t.TempDir(), "missing", "helper.sock")
	release, err := AcquireVMKernelStartupGuard(cfg)
	if err != nil {
		t.Fatal("ordinary deployment was blocked", err)
	}
	release()
	cfg.VMKernel.Enabled = true
	if release, err = AcquireVMKernelStartupGuard(cfg); err == nil {
		release()
		t.Fatal("enabled VM deployment accepted unavailable coordinator")
	}
	cfg.VMKernel.HelperSocket = filepath.Join(t.TempDir(), "helper.sock")
	guard, err := AcquireVMKernelStartupGuard(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer guard()
	platform, err := acquireVMKernelCoordinator(cfg.VMKernel.HelperSocket)
	if err != nil {
		t.Fatal(err)
	}
	platform()
	other, err := openVMCoordinatorLock(cfg.VMKernel.HelperSocket + ".coordinator.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if lockVMCoordinatorFile(other) == nil {
		t.Fatal("startup guard lost lock after platform release")
	}
	guard()
	if err := lockVMCoordinatorFile(other); err != nil {
		t.Fatal("startup guard did not release on shutdown", err)
	}
}

func TestVMKernelCoordinatorSingleOwnerAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "helper.sock")
	release, err := acquireVMKernelCoordinator(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other, err := openVMCoordinatorLock(path + ".coordinator.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if lockVMCoordinatorFile(other) == nil {
		t.Fatal("second coordinator took active lease")
	}
	shared, err := acquireVMKernelCoordinator(path)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if lockVMCoordinatorFile(other) == nil {
		t.Fatal("first platform release dropped shared lease")
	}
	shared()
	if err := lockVMCoordinatorFile(other); err != nil {
		t.Fatal("last platform did not release lock", err)
	}
}

func TestVMKernelCoordinatorRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "helper.sock")
	if err := os.Symlink(target, path+".coordinator.lock"); err != nil {
		t.Fatal(err)
	}
	if release, err := acquireVMKernelCoordinator(path); err == nil {
		release()
		t.Fatal("followed lock symlink")
	}
}
