package vmruntime

import "github.com/google/uuid"

// Client identity belongs to the account instance, not a process or a VM disk.
func (h *Helper) ensureClientIdentity(dir string, s *State) error {
	if s.ClientIdentity != nil {
		parsed, err := uuid.Parse(s.ClientIdentity.DeviceID)
		if err != nil || parsed == uuid.Nil || parsed.String() != s.ClientIdentity.DeviceID {
			return fault("client_identity_invalid")
		}
		return h.ensureDesktopUserAgent(dir, s)
	}
	// Once configuration has been projected, inventing another identity would
	// silently change the account's device on recovery.
	if s.ContainerID != "" || s.DesiredCredentialRevision != "" {
		return fault("client_identity_missing")
	}
	s.ClientIdentity = &ClientIdentity{DeviceID: uuid.NewString()}
	return h.ensureDesktopUserAgent(dir, s)
}
