package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"math/big"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

func shuffleBorrowEntries(entries []int64) error {
	for i := len(entries) - 1; i > 0; i-- {
		n, e := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if e != nil {
			return e
		}
		j := int(n.Int64())
		entries[i], entries[j] = entries[j], entries[i]
	}
	return nil
}

// Randomize candidates and find a complete one-to-one assignment. History is
// audit data only: previously used proxies participate on equal terms.
// Every account switches away from its current proxy, including in a full pool.
func rotationMatching(ctx context.Context, ids, entries []int64, current map[int64]int64) (map[int64]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n, m := len(ids), len(entries)
	if m < n {
		return nil, service.ErrBorrowRotationNoProxy
	}
	entries = append([]int64(nil), entries...)
	if err := shuffleBorrowEntries(entries); err != nil {
		return nil, err
	}
	// Prefer a free candidate before moving another account. Dense pools usually
	// finish with a single scan per source; augmenting paths fix blocked swaps.
	owners := make([]int, m)
	var assign func(int, []bool) bool
	assign = func(row int, visited []bool) bool {
		if ctx.Err() != nil {
			return false
		}
		for j, entry := range entries {
			if !visited[j] && owners[j] == 0 && current[ids[row-1]] != entry {
				owners[j] = row
				return true
			}
		}
		for j, entry := range entries {
			if visited[j] || current[ids[row-1]] == entry {
				continue
			}
			visited[j] = true
			if assign(owners[j], visited) {
				owners[j] = row
				return true
			}
		}
		return false
	}
	for row := 1; row <= n; row++ {
		if !assign(row, make([]bool, m)) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, service.ErrBorrowRotationNoProxy
		}
	}
	result := make(map[int64]int64, n)
	for j, row := range owners {
		if row > 0 {
			result[ids[row-1]] = entries[j]
		}
	}
	return result, nil
}

// Seed the TLS hashes of existing bindings, including a database just upgraded
// from migration 276. This runs under the same pool lock as all allocations.
func seedBorrowFingerprintHistory(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT b.account_id,b.entry_id,b.device_id FROM ticket_borrow_proxy_bindings b LEFT JOIN ticket_borrow_fingerprint_history h ON h.fingerprint_hash=encode(sha256(convert_to(b.device_id,'UTF8')),'hex') WHERE h.tls_profile_hash IS NULL`)
	if err != nil {
		return err
	}
	type binding struct {
		id, entry int64
		device    string
	}
	bindings := []binding{}
	for rows.Next() {
		var b binding
		if err = rows.Scan(&b.id, &b.entry, &b.device); err != nil {
			rows.Close()
			return err
		}
		bindings = append(bindings, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range bindings {
		hash, tls := service.BorrowFingerprintHashes(b.device)
		_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_fingerprint_history(fingerprint_hash,tls_profile_hash,account_id,entry_id) VALUES($1,$2,$3,$4) ON CONFLICT(fingerprint_hash) DO UPDATE SET tls_profile_hash=EXCLUDED.tls_profile_hash`, hash, tls, b.id, b.entry)
		if err != nil {
			return err
		}
	}
	return nil
}
func newBorrowFingerprint(ctx context.Context, tx *sql.Tx, id, entry int64) (string, error) {
	device := uuid.NewString()
	hash, tls := service.BorrowFingerprintHashes(device)
	// TLS profiles may recur across rotations. Recording history must never
	// require an unused profile before a manual or automatic rotation can run.
	_, err := tx.ExecContext(ctx, `INSERT INTO ticket_borrow_fingerprint_history(fingerprint_hash,tls_profile_hash,account_id,entry_id) VALUES($1,$2,$3,$4) ON CONFLICT(fingerprint_hash) DO NOTHING`, hash, tls, id, entry)
	return device, err
}

func recordBorrowProxyUse(ctx context.Context, tx *sql.Tx, id, entry int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ticket_borrow_proxy_usage(account_id,entry_id) VALUES($1,$2) ON CONFLICT(account_id,entry_id) DO UPDATE SET last_used_at=clock_timestamp(),use_count=ticket_borrow_proxy_usage.use_count+1`, id, entry)
	return err
}
