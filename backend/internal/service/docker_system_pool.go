package service

import (
	"context"
	"slices"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const DockerSystemProfileAuto = "auto"

// DockerSystemProfile describes an operator-owned, locally verified template.
// Availability is local deployment state, never an account or upstream probe.
type DockerSystemProfile struct {
	ID                string `json:"id"`
	DisplayName       string `json:"display_name"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type DockerSystemPoolProvider interface {
	ListDockerSystemProfiles(context.Context) ([]DockerSystemProfile, error)
}

// DockerSystemBindingRepository deliberately does not expand AccountRepository.
// The empty key counts legacy bindings; all non-deleted Docker accounts count.
type DockerSystemBindingRepository interface {
	AggregateDockerSystemBindings(context.Context) (map[string]int64, error)
}

// DockerSystemSelection carries only trusted, validated create-time candidates.
// The repository chooses and persists the actual profile under its transaction lock.
type DockerSystemSelection struct {
	Requested  string
	Candidates []string
}

// DockerSystemProfileIDs returns a fresh slice in deterministic allocation order.
func DockerSystemProfileIDs() []string {
	return []string{"centos-stream-9", "ubuntu-24.04", "debian-12", "archlinux", "fedora-44"}
}

func ValidDockerSystemProfileID(id string) bool {
	return slices.Contains(DockerSystemProfileIDs(), id)
}

// ConfigureAdminDockerSystemPoolProvider must be called during composition, before
// the admin service starts serving requests. A missing provider fails closed.
func ConfigureAdminDockerSystemPoolProvider(admin AdminService, provider DockerSystemPoolProvider) {
	if implementation, ok := admin.(*adminServiceImpl); ok {
		implementation.dockerSystemPool = provider
	}
}

func normalizeDockerSystemSelection(mode, requested string) (string, error) {
	if mode != AccountExecutionModeDockerKernel {
		if requested != "" {
			return "", infraerrors.BadRequest("DOCKER_SYSTEM_PROFILE_UNSUPPORTED", "docker_system_profile requires Docker kernel execution")
		}
		return "", nil
	}
	if requested == "" || requested == DockerSystemProfileAuto {
		return DockerSystemProfileAuto, nil
	}
	if !ValidDockerSystemProfileID(requested) {
		return "", infraerrors.BadRequest("DOCKER_SYSTEM_PROFILE_INVALID", "unknown Docker system profile")
	}
	return requested, nil
}

func (s *adminServiceImpl) prepareDockerSystemSelection(ctx context.Context, input *CreateAccountInput, account *Account) error {
	requested, err := normalizeDockerSystemSelection(account.EffectiveExecutionMode(), input.DockerSystemProfile)
	if err != nil || requested == "" {
		return err
	}
	if s.dockerSystemPool == nil {
		return infraerrors.ServiceUnavailable("DOCKER_SYSTEM_POOL_UNAVAILABLE", "Docker system pool is unavailable")
	}
	profiles, err := s.dockerSystemPool.ListDockerSystemProfiles(ctx)
	if err != nil {
		return err
	}
	available := make(map[string]bool, len(profiles))
	for _, profile := range profiles {
		if !ValidDockerSystemProfileID(profile.ID) {
			continue
		}
		if _, duplicate := available[profile.ID]; duplicate {
			return infraerrors.ServiceUnavailable("DOCKER_SYSTEM_POOL_INVALID", "Docker system pool contains duplicate profiles")
		}
		available[profile.ID] = profile.Available
	}
	candidates := make([]string, 0, len(available))
	for _, id := range DockerSystemProfileIDs() {
		if available[id] {
			candidates = append(candidates, id)
		}
	}
	if requested != DockerSystemProfileAuto && !available[requested] {
		return infraerrors.BadRequest("DOCKER_SYSTEM_PROFILE_UNAVAILABLE", "requested Docker system profile is unavailable")
	}
	if len(candidates) == 0 {
		return infraerrors.ServiceUnavailable("DOCKER_SYSTEM_POOL_EMPTY", "no Docker system profile is available")
	}
	account.DockerSystemSelection = &DockerSystemSelection{Requested: requested, Candidates: candidates}
	return nil
}
