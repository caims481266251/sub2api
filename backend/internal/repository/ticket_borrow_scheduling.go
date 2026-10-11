package repository

import "context"

func (r *ticketBorrowRepository) EnabledTargetIDs(ctx context.Context) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT t.account_id FROM ticket_borrow_targets t
 JOIN accounts a ON a.id=t.account_id WHERE t.enabled AND a.deleted_at IS NULL ORDER BY t.account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
