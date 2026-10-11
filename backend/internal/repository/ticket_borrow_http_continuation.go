package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Bindings identify the prior effective session; they no longer pin admission.
// A response is authoritative after public owner validation, even if the client
// session has moved since that response. Never overwrite historical responses.
func (r *ticketBorrowRepository) latestBusinessRoute(ctx context.Context, db borrowQueryer, id int64, identity, response, session, fresh string) (*service.BorrowContinuationRoute, error) {
	var bound, effective string
	key := response
	if key == "" {
		key = session
	}
	if key != "" {
		err := db.QueryRowContext(ctx, `SELECT b.route_id,b.effective_session_id
 FROM ticket_borrow_sessions b JOIN ticket_borrow_routes r ON r.id=b.route_id AND r.target_id=b.target_id
 WHERE b.target_id=$1 AND b.session_hash=$2 AND NOT r.validation_only`, id, key).Scan(&bound, &effective)
		if errors.Is(err, sql.ErrNoRows) {
			if response != "" {
				return nil, service.ErrBorrowUnavailable
			}
		} else if err != nil {
			return nil, err
		}
	}
	route, err := r.readRoute(ctx, db, id, identity, "")
	if err != nil {
		return nil, err
	}
	restarted := bound != "" && bound != route.ID
	if restarted {
		effective = fresh
	}
	return &service.BorrowContinuationRoute{Route: route, SessionID: effective, Restarted: restarted}, nil
}

func (r *ticketBorrowRepository) CheckHTTPContinuation(ctx context.Context, id int64, identity, response, session string) error {
	if response == "" {
		return service.ErrBorrowUnavailable
	}
	_, err := r.latestBusinessRoute(ctx, r.db, id, identity, response, session, "")
	return err
}

func (r *ticketBorrowRepository) CheckLatestRoute(ctx context.Context, id int64, identity, session string) error {
	_, err := r.latestBusinessRoute(ctx, r.db, id, identity, "", session, "")
	return err
}

func (r *ticketBorrowRepository) ReserveHTTPContinuation(ctx context.Context, id int64, identity, response, session, fresh, lease string) (*service.BorrowContinuationRoute, error) {
	if response == "" {
		return nil, service.ErrBorrowUnavailable
	}
	return r.reserveLatestBusinessRoute(ctx, id, identity, response, session, fresh, lease)
}

func (r *ticketBorrowRepository) ReserveLatestRoute(ctx context.Context, id int64, identity, session, fresh, lease string) (*service.BorrowContinuationRoute, error) {
	return r.reserveLatestBusinessRoute(ctx, id, identity, "", session, fresh, lease)
}

func (r *ticketBorrowRepository) reserveLatestBusinessRoute(ctx context.Context, id int64, identity, response, session, fresh, lease string) (*service.BorrowContinuationRoute, error) {
	if fresh == "" || lease == "" {
		return nil, service.ErrBorrowUnavailable
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var locked int64
	if err = tx.QueryRowContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
		return nil, err
	}
	// Re-select after the account queue. The shared target lock serializes
	// publication, session advancement and lease creation across instances.
	choice, err := r.latestBusinessRoute(ctx, tx, id, identity, response, session, fresh)
	if err != nil {
		return nil, err
	}
	if response == "" && session != "" {
		// This is a client-session hash, never an immutable response-ID hash.
		// Concurrent session-only requests reuse the identity chosen by the first.
		_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_sessions(target_id,session_hash,route_id,effective_session_id)
 VALUES($1,$2,$3,$4) ON CONFLICT(target_id,session_hash) DO UPDATE SET route_id=EXCLUDED.route_id,effective_session_id=EXCLUDED.effective_session_id`, id, session, choice.Route.ID, choice.SessionID)
		if err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_route_leases(id,target_id,route_id) VALUES($1,$2,$3)`, lease, id, choice.Route.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return choice, nil
}

func (r *ticketBorrowRepository) BindHTTPContinuationResponse(ctx context.Context, id int64, response, route, effective string) error {
	var bound, session string
	err := r.db.QueryRowContext(ctx, `INSERT INTO ticket_borrow_sessions(target_id,session_hash,route_id,effective_session_id)
 VALUES($1,$2,$3,$4) ON CONFLICT(target_id,session_hash) DO UPDATE SET session_hash=EXCLUDED.session_hash
 RETURNING route_id,effective_session_id`, id, response, route, effective).Scan(&bound, &session)
	if err != nil {
		return err
	}
	if bound != route || session != effective {
		return service.ErrBorrowUnavailable
	}
	return nil
}
