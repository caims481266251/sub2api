package vmruntime

import "github.com/google/uuid"

// Client identity belongs to the account instance, not a process or a VM disk.
func (h *Helper) ensureClientIdentity(_ string, s *State) error {
	if s.ClientIdentity != nil {
		parsed, err := uuid.Parse(s.ClientIdentity.DeviceID)
		if err != nil || parsed.String() != s.ClientIdentity.DeviceID {
			return fault("client_identity_invalid")
		}
		return nil
	}
	s.ClientIdentity = &ClientIdentity{DeviceID: uuid.NewString()}
	return nil
}
