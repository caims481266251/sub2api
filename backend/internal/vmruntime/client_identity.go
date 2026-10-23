package vmruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// ensureClientIdentity migrates the existing kernel installation ID without
// changing the identity seen by the provider. The helper persists the manifest;
// the runner owns creation and permissions of the legacy installation file.
func (h *Helper) ensureClientIdentity(dir string, s *State) error {
	if s.ClientIdentity != nil {
		parsed, e := uuid.Parse(s.ClientIdentity.DeviceID)
		if e != nil || parsed.String() != s.ClientIdentity.DeviceID {
			return fault("client_identity_invalid")
		}
	}
	kvmDir := filepath.Join(dir, "kvm")
	path := filepath.Join(kvmDir, "codex-installation-id")
	if e := rejectSymlink(kvmDir); e != nil {
		return e
	}
	if e := rejectSymlink(path); e != nil {
		return e
	}
	raw, e := os.ReadFile(path)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return fault("client_identity_unreadable")
	}
	device := ""
	if e == nil {
		parsed, parseErr := uuid.Parse(strings.TrimSpace(string(raw)))
		if parseErr != nil {
			return fault("client_identity_invalid")
		}
		device = parsed.String()
	}
	if s.ClientIdentity != nil {
		if device != "" && device != s.ClientIdentity.DeviceID {
			return fault("client_identity_conflict")
		}
		return nil
	}
	if device == "" {
		device = uuid.NewString()
	}
	s.ClientIdentity = &ClientIdentity{DeviceID: device}
	return nil
}
