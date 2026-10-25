package repository

import (
	"context"
	"database/sql"
)

// Legacy bindings are retained only for audit. Candidate allocation never writes
// account bindings, even when pool membership or source selection changes.
func (r *ticketBorrowRepository) reconcileProxyBindings(context.Context) error { return nil }
func reconcileBorrowProxyBindings(context.Context, *sql.Tx) error              { return nil }
