package repository

import (
	"context"
	"database/sql"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type proxyPoolRepository struct {
	db        *sql.DB
	encryptor service.OpenAICredentialEncryptor
}

func NewProxyPoolRepository(db *sql.DB, encryptor service.OpenAICredentialEncryptor) service.ProxyPoolRepository {
	return &proxyPoolRepository{db: db, encryptor: encryptor}
}

func loadProxyPool(ctx context.Context, tx *sql.Tx) (service.ProxyPoolState, error) {
	state := service.ProxyPoolState{Entries: []service.ProxyPoolEntry{}}
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM proxy_pool_state WHERE id=1`).Scan(&state.Revision); err != nil {
		return state, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,identity_hash,protocol,host,port,authenticated,enabled,created_at FROM proxy_pool_entries WHERE retired_at IS NULL ORDER BY id`)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var e service.ProxyPoolEntry
		if err = rows.Scan(&e.ID, &e.Identity, &e.Protocol, &e.Host, &e.Port, &e.Authenticated, &e.Enabled, &e.CreatedAt); err != nil {
			return state, err
		}
		state.Entries = append(state.Entries, e)
	}
	return state, rows.Err()
}
func (r *proxyPoolRepository) Load(ctx context.Context) (service.ProxyPoolState, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return service.ProxyPoolState{}, err
	}
	defer tx.Rollback()
	state, err := loadProxyPool(ctx, tx)
	if err != nil {
		return state, err
	}
	return state, tx.Commit()
}
func lockProxyPool(ctx context.Context, tx *sql.Tx, revision int64) error {
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM proxy_pool_state WHERE id=1 FOR UPDATE`).Scan(&current); err != nil {
		return err
	}
	if current != revision {
		return service.ErrProxyPoolConflict
	}
	return nil
}
func (r *proxyPoolRepository) Apply(ctx context.Context, revision int64, mode string, entries []service.ProxyPoolEntry) (service.ProxyPoolState, error) {
	empty := service.ProxyPoolState{}
	if len(entries) == 0 || len(entries) > service.ProxyPoolMaxEntries || (mode != "append" && mode != "replace") {
		return empty, service.ErrProxyPoolInvalid
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if err = lockProxyPool(ctx, tx, revision); err != nil {
		return empty, err
	}
	identities := make([]string, 0, len(entries))
	changed := int64(0)
	for _, e := range entries {
		identities = append(identities, e.Identity)
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT retired_at IS NULL FROM proxy_pool_entries WHERE identity_hash=$1`, e.Identity).Scan(&active)
		if err != nil && err != sql.ErrNoRows {
			return empty, err
		}
		if err == nil && active {
			continue
		}
		secret, sealErr := r.encryptor.Encrypt(e.URL)
		if sealErr != nil {
			return empty, service.ErrProxyPoolUnavailable
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO proxy_pool_entries(identity_hash,protocol,host,port,authenticated,credential_ciphertext) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(identity_hash) DO UPDATE SET retired_at=NULL`, e.Identity, e.Protocol, e.Host, e.Port, e.Authenticated, secret)
		if err != nil {
			return empty, err
		}
		changed++
	}
	if mode == "replace" {
		result, e := tx.ExecContext(ctx, `UPDATE proxy_pool_entries SET retired_at=clock_timestamp() WHERE retired_at IS NULL AND NOT(identity_hash=ANY($1::text[]))`, pq.Array(identities))
		if e != nil {
			return empty, e
		}
		n, _ := result.RowsAffected()
		changed += n
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM proxy_pool_entries WHERE retired_at IS NULL`).Scan(&count); err != nil {
		return empty, err
	}
	if count > service.ProxyPoolMaxEntries {
		return empty, service.ErrProxyPoolInvalid
	}
	if changed > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE proxy_pool_state SET revision=revision+1,updated_at=clock_timestamp() WHERE id=1`); err != nil {
			return empty, err
		}
	}
	state, err := loadProxyPool(ctx, tx)
	if err != nil {
		return empty, err
	}
	return state, tx.Commit()
}
func (r *proxyPoolRepository) SetEnabled(ctx context.Context, revision, id int64, enabled bool) (service.ProxyPoolState, error) {
	empty := service.ProxyPoolState{}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if err = lockProxyPool(ctx, tx, revision); err != nil {
		return empty, err
	}
	var current bool
	err = tx.QueryRowContext(ctx, `SELECT enabled FROM proxy_pool_entries WHERE id=$1 AND retired_at IS NULL`, id).Scan(&current)
	if err == sql.ErrNoRows {
		return empty, service.ErrProxyPoolConflict
	}
	if err != nil {
		return empty, err
	}
	if current != enabled {
		if _, err = tx.ExecContext(ctx, `UPDATE proxy_pool_entries SET enabled=$2 WHERE id=$1`, id, enabled); err != nil {
			return empty, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE proxy_pool_state SET revision=revision+1,updated_at=clock_timestamp() WHERE id=1`); err != nil {
			return empty, err
		}
	}
	state, err := loadProxyPool(ctx, tx)
	if err != nil {
		return empty, err
	}
	return state, tx.Commit()
}
