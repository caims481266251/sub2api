package vmruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
)

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
