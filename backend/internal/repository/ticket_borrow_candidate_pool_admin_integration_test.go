//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func poolAdminFixture(t *testing.T, slots int) *candidatePoolFixture {
	t.Helper()
	f := newCandidatePoolFixture(t, 1, slots)
	var old service.BorrowCandidatePoolConfig
	require.NoError(t, integrationDB.QueryRow(`SELECT capacity,revision FROM ticket_borrow_candidate_pool_control WHERE id=1`).Scan(&old.Capacity, &old.Revision))
	t.Cleanup(func() {
		_, err := integrationDB.Exec(`UPDATE ticket_borrow_candidate_pool_control SET capacity=$1,revision=$2 WHERE id=1`, old.Capacity, old.Revision)
		require.NoError(t, err)
	})
	return f
}

func poolAdminPage(t *testing.T, f *candidatePoolFixture) *service.BorrowCandidatePoolPage {
	t.Helper()
	v, err := f.r.CandidatePoolPage(t.Context(), service.BorrowCandidatePoolQuery{})
	require.NoError(t, err)
	return v
}

func TestBorrowPoolAdminResizeDrainsWithoutInvalidatingWork(t *testing.T) {
	f := poolAdminFixture(t, 4)
	a := f.targets[0]
	settings, err := f.r.Settings(t.Context())
	require.NoError(t, err)
	first := f.enqueue(t, f.reserve(t))
	claimed, err := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, err)
	require.Equal(t, first.ID, claimed.ID)
	f.enqueue(t, f.reserve(t))
	pending := f.reserve(t)
	page := poolAdminPage(t, f)
	require.Equal(t, 64, page.Summary.Capacity)
	require.Equal(t, 3, page.Summary.Occupied)
	require.Equal(t, 1, page.Summary.Acquiring)
	require.Equal(t, 1, page.Summary.Queued)
	require.Equal(t, 1, page.Summary.Claimed)
	saved, err := f.r.SaveCandidatePoolConfig(t.Context(), service.BorrowCandidatePoolConfig{Capacity: 1, Revision: page.Summary.Revision})
	require.NoError(t, err)
	_, err = f.r.SaveCandidatePoolConfig(t.Context(), service.BorrowCandidatePoolConfig{Capacity: 128, Revision: page.Summary.Revision})
	require.ErrorIs(t, err, service.ErrBorrowConflict, "stale writes cannot overwrite a newer limit")
	require.NoError(t, f.r.CheckCandidatePoolAcquisition(t.Context(), pending))
	f.enqueue(t, pending)
	require.NoError(t, f.r.SweepCandidatePool(t.Context()))
	require.Equal(t, 3, poolAdminPage(t, f).Summary.Occupied, "shrinking cannot evict current candidates")
	reserved, err := f.r.ReserveCandidatePoolFetch(t.Context(), uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, reserved)
	second, err := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, err)
	require.NotNil(t, second, "existing queued candidates can still be verified above capacity")
	route := poolCandidateRoute(a, claimed)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	require.NoError(t, f.r.Publish(ctx, route, a.Lease), "a resize cannot fence an already claimed success")
	passBorrowFixtureQuality(t)
	require.NoError(t, f.r.FinishCandidatePool(ctx, claimed.ID, a.Lease, "target_probe_passed"))
	lease := uuid.NewString()
	_, err = f.r.ReserveRoute(ctx, a.TargetID, a.Identity, "", "", lease)
	require.NoError(t, err)
	_, err = f.r.SaveCandidatePoolConfig(t.Context(), service.BorrowCandidatePoolConfig{Capacity: 128, Revision: saved.Revision})
	require.NoError(t, err)
	require.NoError(t, f.r.RenewRoute(ctx, lease), "resizing preserves admitted business requests")
	require.NoError(t, f.r.ReleaseRoute(ctx, lease))
	now, err := f.r.Settings(t.Context())
	require.NoError(t, err)
	require.Equal(t, settings.Revision, now.Revision)
	require.NoError(t, f.r.CheckCandidateEnvironment(ctx, second.ID))
	restarted := NewTicketBorrowRepository(integrationDB, f.r.encryptor).(*ticketBorrowRepository)
	got, err := restarted.CandidatePoolPage(t.Context(), service.BorrowCandidatePoolQuery{})
	require.NoError(t, err)
	require.Equal(t, 128, got.Summary.Capacity, "capacity persists across service instances")
}

func TestBorrowPoolAdminCapacityAdmissionAcrossInstances(t *testing.T) {
	f := poolAdminFixture(t, 32)
	page := poolAdminPage(t, f)
	saved, err := f.r.SaveCandidatePoolConfig(t.Context(), service.BorrowCandidatePoolConfig{Capacity: 3, Revision: page.Summary.Revision})
	require.NoError(t, err)
	restarted := NewTicketBorrowRepository(integrationDB, f.r.encryptor).(*ticketBorrowRepository)
	var wg sync.WaitGroup
	reservations := make(chan *service.BorrowPoolAcquisition, 12)
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.r
			if i%2 == 1 {
				r = restarted
			}
			p, e := r.ReserveCandidatePoolFetch(t.Context(), uuid.NewString(), uuid.NewString())
			errs <- e
			if p != nil {
				reservations <- p
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	require.Len(t, reservations, 3)
	var pending []service.BorrowPoolAcquisition
	for range 3 {
		pending = append(pending, *<-reservations)
	}
	for _, p := range pending {
		f.enqueue(t, p)
	}
	_, err = f.r.SaveCandidatePoolConfig(t.Context(), service.BorrowCandidatePoolConfig{Capacity: 5, Revision: saved.Revision})
	require.NoError(t, err)
	f.enqueue(t, f.reserve(t))
	f.enqueue(t, f.reserve(t))
	require.Equal(t, 5, poolAdminPage(t, f).Summary.Occupied)
	blocked, err := f.r.ReserveCandidatePoolFetch(t.Context(), uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, blocked)
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), pending[0].ID, pending[0].Lease, "candidate_pool_no_consumers"))
	f.reserve(t)
	require.Equal(t, 5, poolAdminPage(t, f).Summary.Occupied, "finished candidates free capacity for new work")
}

func TestBorrowPoolAdminSnapshotFilteringDetailsAndRedaction(t *testing.T) {
	f := poolAdminFixture(t, 4)
	a := f.targets[0]
	c := f.enqueue(t, f.reserve(t))
	claimed, err := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, err)
	route := poolCandidateRoute(a, claimed)
	require.NoError(t, f.r.Publish(service.WithBorrowAcquisition(t.Context(), a), route, a.Lease))
	passBorrowFixtureQuality(t)
	// Observation may contain extra fields from an older/newer binary. Only the
	// typed public allowlist may cross this management API boundary.
	_, err = integrationDB.Exec(`INSERT INTO ticket_borrow_events(target_id,source_id,stage,reason,details)
 VALUES($1,$2,'target','target_probe_passed',jsonb_build_object('candidate_id',$3::text,'probe_verdict','healthy',
 'mint',jsonb_build_object('duration_ms',123,'retry_count',0,'attempts','[]'::jsonb),
 'source',jsonb_build_object('configured_proxy_hash','proxy-hash','profile_hash','profile-hash','password','do-not-expose'),
 'secret','do-not-expose','owner_token',$4::text,'cookie','do-not-expose','batch_id',$4::text))`, a.TargetID, c.SourceID, c.ID, a.Lease)
	require.NoError(t, err)
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), c.ID, a.Lease, "target_probe_passed"))
	detail, err := f.r.CandidatePoolDetail(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "finished", detail.State)
	require.Equal(t, route.ID, detail.RouteID)
	require.Equal(t, "healthy", detail.Details.ProbeVerdict)
	require.EqualValues(t, 123, detail.Details.Mint.DurationMS)
	require.Equal(t, "proxy-hash", detail.Details.Source.ProxyHash)
	require.NotNil(t, detail.Details.PoolEnqueuedAt)
	require.Empty(t, detail.Details.BatchID)
	raw, err := json.Marshal(detail)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "do-not-expose")
	require.NotContains(t, string(raw), a.Lease)
	page, err := f.r.CandidatePoolPage(t.Context(), service.BorrowCandidatePoolQuery{State: "finished", SourceAccountID: detail.SourceAccountID, TargetID: a.TargetID, Search: c.ID, PageSize: 1})
	require.NoError(t, err)
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	require.Zero(t, page.Summary.Occupied)
	page, err = f.r.CandidatePoolPage(t.Context(), service.BorrowCandidatePoolQuery{State: "finished", Search: "%_"})
	require.NoError(t, err)
	require.Zero(t, page.Total, "search characters are literal, not SQL wildcards")
	page, err = f.r.CandidatePoolPage(t.Context(), service.BorrowCandidatePoolQuery{State: "all", Page: 2, PageSize: 1})
	require.NoError(t, err)
	require.Empty(t, page.Items)
	_, err = f.r.CandidatePoolDetail(context.Background(), uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowPoolNotFound)
}
