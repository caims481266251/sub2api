//go:build !windows

package vmruntime

import (
	"os"
	"path/filepath"
	"syscall"
)

// PrepareCoordinatorLock runs in the privileged helper before it accepts
// connections. Preserve the inode: replacing a live flock file would permit
// two application coordinators to believe they own the same VM instances.
func PrepareCoordinatorLock(socket string, serviceUID int) error {
	if !filepath.IsAbs(socket) || serviceUID < 0 || uint64(serviceUID) > 1<<31-1 {
		return fault("coordinator_lock_configuration_invalid")
	}
	f, err := os.OpenFile(socket+".coordinator.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return fault("coordinator_lock_unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fault("coordinator_lock_invalid")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return fault("coordinator_lock_invalid")
	}
	if int(stat.Uid) != serviceUID {
		if err := f.Chown(serviceUID, -1); err != nil {
			return fault("coordinator_lock_owner_failed")
		}
	}
	if err := f.Chmod(0600); err != nil {
		return fault("coordinator_lock_permissions_failed")
	}
	return nil
}
