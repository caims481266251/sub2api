package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type dockerSystemCountsFixture struct {
	AccountRepository
	calls  int
	scoped bool
	err    error
}

func (r *dockerSystemCountsFixture) AggregateDockerSystemBindings(ctx context.Context) (map[string]int64, error) {
	r.calls++
	_, r.scoped = ObserverGroupIDs(ctx)
	return map[string]int64{"": 7, "debian-12": 9}, r.err
}
func TestDockerSystemPoolSnapshotUsesScopedAggregate(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		repo := &dockerSystemCountsFixture{}
		svc := &adminServiceImpl{accountRepo: repo, dockerSystemPool: dockerSystemPoolFixture()}
		ctx := t.Context()
		if scoped {
			ctx = WithObserverScope(ctx, []int64{1})
		}
		out, err := svc.GetDockerSystemPool(ctx)
		require.NoError(t, err)
		require.Len(t, out.Profiles, 5)
		require.Equal(t, int64(9), out.Profiles[2].BoundAccountCount)
		require.Equal(t, int64(7), out.LegacyBoundAccountCount)
		require.Equal(t, scoped, repo.scoped)
		require.Equal(t, 1, repo.calls)
		if scoped {
			require.Equal(t, "visible", out.CountScope)
		} else {
			require.Equal(t, "all", out.CountScope)
		}
		repo.err = errors.New("database unavailable")
		out, err = svc.GetDockerSystemPool(ctx)
		require.Error(t, err)
		require.Nil(t, out)
	}
}
func TestDockerSystemRuntimeBindingPersistsAcrossRefresh(t *testing.T) {
	for _, id := range []string{"", "ubuntu-24.04", "archlinux"} {
		t.Run(id, func(t *testing.T) {
			s, a, f := vmLifecycleSetup(t)
			if id != "" {
				a.DockerSystemProfile = &id
			}
			s.prepare(t.Context(), a)
			require.True(t, VMKernelRuntimeReady(a))
			want := id
			if want == "" {
				want = "default"
			}
			require.Equal(t, want, f.state.Spec.Template)
			identity := f.state.ClientIdentity.DeviceID
			a.Credentials["access_token"] = "rotated-fixture-token"
			s.prepare(t.Context(), a)
			require.Equal(t, want, f.state.Spec.Template)
			require.Equal(t, identity, f.state.ClientIdentity.DeviceID)
			require.True(t, VMKernelRuntimeReady(a))
			before := len(f.requests)
			s.prepare(t.Context(), a)
			require.Equal(t, before+1, len(f.requests))
			require.Equal(t, "inspect", f.requests[len(f.requests)-1].Operation)
		})
	}
}
func TestDockerSystemRuntimeRejectsMismatchedBindingWithoutRecreate(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	id := "debian-12"
	a.DockerSystemProfile = &id
	s.prepare(t.Context(), a)
	require.True(t, VMKernelRuntimeReady(a))
	f.state.Spec.Template = "ubuntu-24.04"
	before := len(f.requests)
	s.prepare(t.Context(), a)
	require.False(t, VMKernelRuntimeReady(a))
	require.Equal(t, before+1, len(f.requests))
	require.Equal(t, "inspect", f.requests[len(f.requests)-1].Operation)
}
