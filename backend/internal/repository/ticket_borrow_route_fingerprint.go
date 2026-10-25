package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Older encrypted route records contain the account template, not the actual
// candidate fingerprint. Recover only from that route's persisted candidate.
func (r *ticketBorrowRepository) openRouteSecret(ctx context.Context, db borrowQueryer, id, encrypted string, secret *service.BorrowRouteSecret) error {
	if err := r.open(encrypted, secret); err != nil {
		return err
	}
	if secret.Fingerprint != nil {
		return nil
	}
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT e.fingerprint FROM ticket_borrow_routes r JOIN ticket_borrow_candidate_environments e ON e.id=r.candidate_id WHERE r.id=$1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		// The send boundary fails closed without substituting an account profile.
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &secret.Fingerprint)
}
