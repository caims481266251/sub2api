//go:build astra_experiment && astra_ready_overlay

package repository

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// Run with the documented diagnostic overlay, not against the production
// implementation. Readiness is scheduling only; all health/wait checks survive.
func TestDirectionsOverlayReadinessPreservesGuards(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		scoped, running, active, upstream bool
		budget                            any
		want                              error
	}{
		{"normal ready", false, true, true, false, nil, service.ErrBorrowRouteReady},
		{"diagnostic ready", true, true, true, false, nil, nil},
		{"disabled account", true, true, false, false, nil, service.ErrBorrowUnavailable},
		{"global pause", true, false, true, false, nil, service.ErrBorrowPaused},
		{"upstream cooldown", true, true, true, true, nil, service.ErrBorrowRecoveryWaiting},
		{"request budget", true, true, true, false, time.Now().Add(time.Minute), service.ErrBorrowRequestBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, m, e := sqlmock.New()
			require.NoError(t, e)
			defer db.Close()
			a := service.BorrowAcquisition{TargetID: 63, Revision: 2, Lease: "directions-test", Identity: "id"}
			ctx := service.WithBorrowAcquisition(context.Background(), a)
			if tc.scoped {
				ctx = service.WithBorrowDirectionsReadyWindow(ctx, a.Lease, time.Now().Add(time.Minute))
			}
			m.ExpectQuery("SELECT s.running").WithArgs(a.TargetID, a.Revision, a.Lease, a.Identity).WillReturnRows(sqlmock.NewRows([]string{"running", "active", "ready", "upstream", "budget", "now"}).AddRow(tc.running, tc.active, true, tc.upstream, tc.budget, time.Now()))
			err := checkBorrowAcquisition(ctx, db, a, true)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
