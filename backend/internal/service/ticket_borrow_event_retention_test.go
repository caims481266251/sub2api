package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type borrowRetentionRepo struct {
	TicketBorrowRepository
	BorrowRotationRepository
	calls       int
	cleanupErr  error
	rotationErr error
}

func (r *borrowRetentionRepo) CleanupEvents(context.Context) error {
	r.calls++
	return r.cleanupErr
}
func (r *borrowRetentionRepo) ProcessRotations(context.Context) error { return r.rotationErr }
func (r *borrowRetentionRepo) Cleanup(context.Context) error          { return nil }
func (r *borrowRetentionRepo) Settings(context.Context) (BorrowSettings, error) {
	return BorrowSettings{Running: false}, nil
}

func TestTicketBorrowEventRetentionRunsWhilePausedOrRotationFails(t *testing.T) {
	for _, rotationErr := range []error{nil, errors.New("rotation unavailable")} {
		r := &borrowRetentionRepo{rotationErr: rotationErr}
		s := NewTicketBorrowService(r, nil, nil)
		s.tick(t.Context())
		require.Equal(t, 1, r.calls)
		require.WithinDuration(t, time.Now().Add(time.Minute), s.nextEventCleanup, time.Second)
		s.tick(t.Context())
		require.Equal(t, 1, r.calls, "ordinary ticks must not repeatedly scan history")
		s.nextEventCleanup = time.Now().Add(-time.Second)
		s.tick(t.Context())
		require.Equal(t, 2, r.calls)
	}
}

func TestTicketBorrowEventRetentionRetriesFailedCleanup(t *testing.T) {
	r := &borrowRetentionRepo{cleanupErr: errors.New("storage unavailable")}
	s := NewTicketBorrowService(r, nil, nil)
	s.cleanupEvents(t.Context())
	require.WithinDuration(t, time.Now().Add(5*time.Second), s.nextEventCleanup, time.Second)
	r.cleanupErr = nil
	s.nextEventCleanup = time.Now().Add(-time.Second)
	s.cleanupEvents(t.Context())
	require.Equal(t, 2, r.calls)
	require.WithinDuration(t, time.Now().Add(time.Minute), s.nextEventCleanup, time.Second)
}
