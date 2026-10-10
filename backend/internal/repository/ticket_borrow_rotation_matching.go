package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"math/big"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
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
	return matchBorrowEntries(ctx, ids, entries, current, false)
}

func matchBorrowEntries(ctx context.Context, ids, entries []int64, current map[int64]int64, allowCurrent bool) (map[int64]int64, error) {
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
		if allowCurrent {
			// The strict pass has already tried to move every account. Prefer
			// a free same-proxy slot before expensive relocation chains; this
			// keeps a large single-proxy pool quadratic instead of cubic.
			for j, entry := range entries {
				if !visited[j] && owners[j] == 0 && current[ids[row-1]] == entry {
					owners[j] = row
					return true
				}
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
		if allowCurrent {
			for j, entry := range entries {
				if visited[j] || current[ids[row-1]] != entry {
					continue
				}
				visited[j] = true
				if owners[j] == 0 || assign(owners[j], visited) {
					owners[j] = row
					return true
				}
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

// Allocate balanced capacities first, then match accounts to those slots.
// Existing bindings outside this batch stay fixed. In a scarce pool, balance
// takes precedence over changing every proxy; every rotation still gets a new
// device identity, even when keeping its only/least-loaded proxy is necessary.
func balancedBorrowMatching(ctx context.Context, ids, entries []int64, current map[int64]int64, loads map[int64]int) (map[int64]int64, error) {
	if len(ids) == 0 {
		return map[int64]int64{}, nil
	}
	if len(entries) == 0 {
		return nil, service.ErrBorrowRotationNoProxy
	}
	entries = append([]int64(nil), entries...)
	if err := shuffleBorrowEntries(entries); err != nil {
		return nil, err
	}
	projected := make(map[int64]int, len(entries))
	for _, entry := range entries {
		projected[entry] = loads[entry]
	}
	slots := make([]int64, 0, len(ids))
	for range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		best := entries[0]
		for _, entry := range entries[1:] {
			if projected[entry] < projected[best] {
				best = entry
			}
		}
		slots = append(slots, best)
		projected[best]++
	}
	result, err := rotationMatching(ctx, ids, slots, current)
	if err != service.ErrBorrowRotationNoProxy {
		return result, err
	}
	return matchBorrowEntries(ctx, ids, slots, current, true)
}

// Call under the pool row lock. Count each outside account once at its planned
// destination, so concurrently draining rotations cannot reserve the same
// apparently empty capacity. Legacy target bindings remain exclusive.
func borrowProxyLoads(ctx context.Context, tx *sql.Tx, ids []int64) ([]int64, map[int64]int, error) {
	rows, err := tx.QueryContext(ctx, `WITH planned AS (
 SELECT b.account_id,COALESCE(x.entry_id,b.entry_id) AS entry_id
 FROM ticket_borrow_proxy_bindings b
 LEFT JOIN ticket_borrow_rotation_assignments x ON x.account_id=b.account_id
 WHERE b.role='source' AND NOT(b.account_id=ANY($1)))
 SELECT p.id,(SELECT count(*) FROM planned a WHERE a.entry_id=p.id)
 FROM proxy_pool_entries p WHERE p.enabled AND p.retired_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_proxy_bindings b WHERE b.entry_id=p.id AND b.role='target')
 ORDER BY p.id`, pq.Array(ids))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var entries []int64
	loads := map[int64]int{}
	for rows.Next() {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, nil, err
		}
		entries = append(entries, id)
		loads[id] = count
	}
	return entries, loads, rows.Err()
}

// Seed the TLS hashes of existing bindings, including a database just upgraded
// from migration 276. This runs under the same pool lock as all allocations.
func seedBorrowFingerprintHistory(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT b.account_id,b.entry_id,b.device_id FROM ticket_borrow_proxy_bindings b LEFT JOIN ticket_borrow_fingerprint_history h ON h.fingerprint_hash=encode(sha256(convert_to(b.device_id,'UTF8')),'hex') WHERE h.client_hello_hash IS NULL AND b.fingerprint_version=1
 UNION SELECT b.account_id,b.entry_id,b.device_id FROM ticket_borrow_rotation_assignments b LEFT JOIN ticket_borrow_fingerprint_history h ON h.fingerprint_hash=encode(sha256(convert_to(b.device_id,'UTF8')),'hex') WHERE h.client_hello_hash IS NULL AND b.fingerprint_version=1`)
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
		structure := service.BorrowLegacyClientHelloHash(b.device)
		_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_fingerprint_history(fingerprint_hash,tls_profile_hash,account_id,entry_id,client_hello_hash) VALUES($1,$2,$3,$4,$5) ON CONFLICT(fingerprint_hash) DO UPDATE SET tls_profile_hash=EXCLUDED.tls_profile_hash,client_hello_hash=EXCLUDED.client_hello_hash WHERE ticket_borrow_fingerprint_history.snapshot IS NULL`, hash, tls, b.id, b.entry, structure)
		if err != nil {
			return err
		}
	}
	return nil
}
func newBorrowFingerprint(ctx context.Context, tx *sql.Tx, id, entry int64) (string, error) {
	return newBorrowFingerprintWithGenerator(ctx, tx, id, entry, service.NewBorrowFingerprint)
}

const borrowFingerprintAttempts = 64

// All callers hold the pool row lock. Candidates reserved for draining jobs
// enter this same history immediately, so concurrent jobs cannot choose them
// as globally unused. Bounded fallback never relaxes template validation.
func newBorrowFingerprintWithGenerator(ctx context.Context, tx *sql.Tx, id, entry int64, generate func() (*service.BorrowFingerprint, error)) (string, error) {
	var best *service.BorrowFingerprint
	var bestOwn, bestTotal int64
	for range borrowFingerprintAttempts {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		candidate, err := generate()
		if err != nil {
			return "", err
		}
		if err = service.ValidateBorrowFingerprint(candidate); err != nil {
			return "", err
		}
		tls, structure := service.BorrowTLSFingerprintHashes(candidate.Profile)
		legacy := service.BorrowLegacyProfileHash(candidate.Profile)
		var own, total int64
		var deviceUsed bool
		err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE account_id=$1),count(*),
 EXISTS(SELECT 1 FROM ticket_borrow_fingerprint_history WHERE fingerprint_hash=encode(sha256(convert_to($5,'UTF8')),'hex'))
 FROM ticket_borrow_fingerprint_history WHERE client_hello_hash=$2 OR tls_profile_hash=$3 OR tls_profile_hash=$4`, id, structure, tls, legacy, candidate.DeviceID).Scan(&own, &total, &deviceUsed)
		if err != nil {
			return "", err
		}
		if deviceUsed {
			continue
		}
		if best == nil || own < bestOwn || (own == bestOwn && total < bestTotal) {
			best, bestOwn, bestTotal = candidate, own, total
		}
		if total == 0 {
			best, bestTotal = candidate, 0
			break
		}
	}
	if best == nil {
		return "", service.ErrBorrowRotationNoFingerprint
	}
	best.Reused = bestTotal > 0
	raw, err := json.Marshal(best)
	if err != nil {
		return "", err
	}
	tls, structure := service.BorrowTLSFingerprintHashes(best.Profile)
	_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_fingerprint_history(fingerprint_hash,tls_profile_hash,account_id,entry_id,snapshot,client_hello_hash)
 VALUES(encode(sha256(convert_to($1,'UTF8')),'hex'),$2,$3,$4,$5::jsonb,$6)`, best.DeviceID, tls, id, entry, string(raw), structure)
	return best.DeviceID, err
}

func recordBorrowProxyUse(ctx context.Context, tx *sql.Tx, id, entry int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ticket_borrow_proxy_usage(account_id,entry_id) VALUES($1,$2) ON CONFLICT(account_id,entry_id) DO UPDATE SET last_used_at=clock_timestamp(),use_count=ticket_borrow_proxy_usage.use_count+1`, id, entry)
	return err
}

// Recheck persisted plans immediately before swapping the whole batch. A lost
// or damaged snapshot cannot replace an otherwise usable current environment.
func validBorrowRotationFingerprints(ctx context.Context, tx *sql.Tx, job string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT x.device_id,x.fingerprint_version,COALESCE(h.snapshot,'null'::jsonb)
 FROM ticket_borrow_rotation_assignments x LEFT JOIN ticket_borrow_fingerprint_history h
 ON h.fingerprint_hash=encode(sha256(convert_to(x.device_id,'UTF8')),'hex') WHERE x.job_id=$1`, job)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var device string
		var version int
		var raw []byte
		if err = rows.Scan(&device, &version, &raw); err != nil {
			return false, err
		}
		if version == 1 {
			continue
		} // Already queued before the upgrade.
		var f *service.BorrowFingerprint
		if version != 2 || json.Unmarshal(raw, &f) != nil || service.ValidateBorrowFingerprint(f) != nil || f.DeviceID != device {
			return false, nil
		}
	}
	return true, rows.Err()
}
