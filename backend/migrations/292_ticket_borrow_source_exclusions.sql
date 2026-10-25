-- Account-level exceptions to dynamic borrowing source groups.
ALTER TABLE ticket_borrow_selection
 ADD COLUMN excluded_account_ids JSONB NOT NULL DEFAULT '[]'::jsonb
 CHECK (jsonb_typeof(excluded_account_ids) = 'array');
