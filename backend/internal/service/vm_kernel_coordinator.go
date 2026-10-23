package service

import (
	"errors"
	"os"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

var vmCoordinator struct {
	sync.Mutex
	file   *os.File
	socket string
	refs   int
}

// AcquireVMKernelStartupGuard prevents an enabled VM deployment from serving
// a healthy HTTP endpoint when its coordinator could not initialize. Keep the
// guard until shutdown so platform services share the same lock throughout.
func AcquireVMKernelStartupGuard(cfg *config.Config) (func(), error) {
	if cfg == nil || !cfg.VMKernel.Enabled {
		return func() {}, nil
	}
	return acquireVMKernelCoordinator(cfg.VMKernel.HelperSocket)
}

// Both platform managers share a process lock adjacent to the private helper
// socket. A second Sub2API process cannot admit another slot for the same guest.
// The lock inode must be retained across restarts, just like the helper lock.
func acquireVMKernelCoordinator(socket string) (func(), error) {
	vmCoordinator.Lock()
	defer vmCoordinator.Unlock()
	if vmCoordinator.file != nil {
		if vmCoordinator.socket != socket {
			return nil, errors.New("vm_kernel_coordinator_socket_conflict")
		}
	} else {
		file, err := openVMCoordinatorLock(socket + ".coordinator.lock")
		if err != nil {
			return nil, errors.New("vm_kernel_coordinator_lock_unavailable")
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			file.Close()
			return nil, errors.New("vm_kernel_coordinator_lock_not_private")
		}
		if err := lockVMCoordinatorFile(file); err != nil {
			file.Close()
			return nil, errors.New("vm_kernel_coordinator_already_running")
		}
		vmCoordinator.file, vmCoordinator.socket = file, socket
	}
	vmCoordinator.refs++
	var once sync.Once
	return func() {
		once.Do(func() {
			vmCoordinator.Lock()
			defer vmCoordinator.Unlock()
			vmCoordinator.refs--
			if vmCoordinator.refs == 0 {
				vmCoordinator.file.Close()
				vmCoordinator.file = nil
				vmCoordinator.socket = ""
			}
		})
	}, nil
}
