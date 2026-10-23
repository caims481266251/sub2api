//go:build !windows

package vmruntime

import (
	"os"
	"syscall"
)

func lockStateFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fault("helper_already_running")
	}
	return nil
}
