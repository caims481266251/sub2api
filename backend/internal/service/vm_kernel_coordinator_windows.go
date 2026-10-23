package service

import (
	"errors"
	"os"
)

// VM execution is Linux-only; ordinary Windows service builds remain supported.
func openVMCoordinatorLock(string) (*os.File, error) {
	return nil, errors.New("vm_kernel_linux_required")
}
func lockVMCoordinatorFile(*os.File) error { return errors.New("vm_kernel_linux_required") }
