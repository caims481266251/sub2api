package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Resolve the existing resident runtime lazily; this adds no second coordinator.
type dockerSystemRuntimeProvider struct{}

func (dockerSystemRuntimeProvider) ListDockerSystemProfiles(ctx context.Context) ([]DockerSystemProfile, error) {
	return LookupVMKernelService(PlatformOpenAI).ListDockerSystemProfiles(ctx)
}

func (s *VMKernelService) ListDockerSystemProfiles(ctx context.Context) ([]DockerSystemProfile, error) {
	if s == nil || s.helper == nil {
		return nil, infraerrors.ServiceUnavailable("DOCKER_SYSTEM_POOL_UNAVAILABLE", "Docker system pool is unavailable")
	}
	profiles, err := s.helper.SystemProfiles(ctx)
	if err != nil {
		return nil, infraerrors.ServiceUnavailable("DOCKER_SYSTEM_POOL_UNAVAILABLE", "Unable to load Docker system pool; check the installed runtime version")
	}
	result := make([]DockerSystemProfile, 0, len(profiles))
	for _, p := range profiles {
		result = append(result, DockerSystemProfile{ID: p.ID, DisplayName: p.DisplayName, Available: p.Available, UnavailableReason: p.UnavailableReason})
	}
	return result, nil
}

type DockerSystemPoolItem struct {
	ProfileID         string `json:"profile_id"`
	DisplayName       string `json:"display_name"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	BoundAccountCount int64  `json:"bound_account_count"`
}
type DockerSystemPoolSnapshot struct {
	Profiles                []DockerSystemPoolItem `json:"profiles"`
	LegacyBoundAccountCount int64                  `json:"legacy_bound_account_count"`
	CheckedAt               time.Time              `json:"checked_at"`
	CountScope              string                 `json:"count_scope"`
}

// GetDockerSystemPool combines local catalog state and one scoped SQL aggregate.
// Never use account response enrichment here: it can probe usage or runtime state.
func (s *adminServiceImpl) GetDockerSystemPool(ctx context.Context) (*DockerSystemPoolSnapshot, error) {
	if s.dockerSystemPool == nil {
		return nil, infraerrors.ServiceUnavailable("DOCKER_SYSTEM_POOL_UNAVAILABLE", "Docker system pool is unavailable")
	}
	repo, ok := s.accountRepo.(DockerSystemBindingRepository)
	if !ok {
		return nil, infraerrors.ServiceUnavailable("DOCKER_SYSTEM_COUNTS_UNAVAILABLE", "Docker system binding counts are unavailable")
	}
	profiles, err := s.dockerSystemPool.ListDockerSystemProfiles(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := repo.AggregateDockerSystemBindings(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]DockerSystemProfile, len(profiles))
	for _, p := range profiles {
		if !ValidDockerSystemProfileID(p.ID) {
			return nil, infraerrors.ServiceUnavailable("DOCKER_SYSTEM_POOL_INVALID", "Invalid Docker system pool")
		}
		if _, exists := byID[p.ID]; exists {
			return nil, infraerrors.ServiceUnavailable("DOCKER_SYSTEM_POOL_INVALID", "Duplicate Docker system profile")
		}
		byID[p.ID] = p
	}
	names := map[string]string{"centos-stream-9": "CentOS Stream 9", "ubuntu-24.04": "Ubuntu 24.04 LTS", "debian-12": "Debian 12", "archlinux": "Arch Linux", "fedora-44": "Fedora 44"}
	out := &DockerSystemPoolSnapshot{Profiles: make([]DockerSystemPoolItem, 0, 5), LegacyBoundAccountCount: counts[""], CheckedAt: time.Now().UTC(), CountScope: "all"}
	if _, scoped := ObserverGroupIDs(ctx); scoped {
		out.CountScope = "visible"
	}
	for _, id := range DockerSystemProfileIDs() {
		p, exists := byID[id]
		if !exists {
			p = DockerSystemProfile{ID: id, DisplayName: names[id], UnavailableReason: "template_not_installed"}
		}
		out.Profiles = append(out.Profiles, DockerSystemPoolItem{ProfileID: id, DisplayName: p.DisplayName, Available: p.Available, UnavailableReason: p.UnavailableReason, BoundAccountCount: counts[id]})
	}
	return out, nil
}
