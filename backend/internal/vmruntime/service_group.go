package vmruntime

import (
	"os"
	"path/filepath"
)

// ConfigureServiceGroup is operator-only and must run before serving requests.
// Only directory traversal is granted; manifests and credentials remain private.
func (h *Helper) ConfigureServiceGroup(gid int) error {
	if gid < 0 || uint64(gid) > uint64(1<<31-1) {
		return fault("service_gid_invalid")
	}
	h.serviceGID = gid
	if gid == 0 {
		return nil
	}
	if os.Geteuid() != 0 {
		return fault("service_group_requires_root")
	}
	if err := serviceDirectory(h.root, 0, gid); err != nil {
		return err
	}
	entries, err := os.ReadDir(h.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(h.root, entry.Name())
		if _, err := h.read(dir); err != nil {
			return fault("manifest_inventory_unreadable")
		}
		if err := h.instanceDir(dir); err != nil {
			return err
		}
		run := filepath.Join(dir, "run")
		if _, err := os.Lstat(run); err == nil {
			if err = h.runDir(run); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
func serviceDirectory(path string, uid, gid int) error {
	if err := rejectSymlink(path); err != nil {
		return err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0710)
}
func (h *Helper) instanceDir(path string) error {
	if err := privateDir(path); err != nil {
		return err
	}
	if h.serviceGID > 0 {
		return serviceDirectory(path, 0, h.serviceGID)
	}
	return nil
}
func (h *Helper) runDir(path string) error {
	if err := privateDir(path); err != nil {
		return err
	}
	if h.serviceGID > 0 {
		return serviceDirectory(path, 10001, h.serviceGID)
	}
	return nil
}
