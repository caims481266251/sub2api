package repository

import (
	"context"
	"math/rand"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowRotationMatchingFullPoolCanRepeat(t *testing.T) {
	current := map[int64]int64{1: 20, 2: 21}
	for range 10 {
		got, err := rotationMatching(t.Context(), []int64{1, 2}, []int64{20, 21}, current)
		require.NoError(t, err)
		require.Equal(t, current[2], got[1])
		require.Equal(t, current[1], got[2])
		current = got
	}
}

func TestTicketBorrowRotationMatchingAgreesWithExhaustiveAssignments(t *testing.T) {
	random := rand.New(rand.NewSource(731))
	for trial := 0; trial < 120; trial++ {
		n, m := 1+random.Intn(4), 1+random.Intn(5)
		ids, entries := make([]int64, n), make([]int64, m)
		current := map[int64]int64{}
		for j := range entries {
			entries[j] = int64(10 + j)
		}
		for i := range ids {
			ids[i] = int64(i + 1)
			if random.Intn(2) == 0 {
				current[ids[i]] = entries[random.Intn(m)]
			}
		}
		var feasible func(int, map[int64]bool) bool
		feasible = func(i int, used map[int64]bool) bool {
			if i == n {
				return true
			}
			for _, entry := range entries {
				if used[entry] || current[ids[i]] == entry {
					continue
				}
				used[entry] = true
				if feasible(i+1, used) {
					return true
				}
				delete(used, entry)
			}
			return false
		}
		possible := feasible(0, map[int64]bool{})
		got, err := rotationMatching(context.Background(), ids, entries, current)
		if !possible {
			require.ErrorIs(t, err, service.ErrBorrowRotationNoProxy)
			continue
		}
		require.NoError(t, err)
		require.Len(t, got, n)
		used := map[int64]bool{}
		for _, id := range ids {
			entry := got[id]
			require.Contains(t, entries, entry)
			require.NotEqual(t, current[id], entry)
			require.False(t, used[entry])
			used[entry] = true
		}
	}
}
