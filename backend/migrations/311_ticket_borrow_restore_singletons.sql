-- A data-only reset can remove these singleton rows while retaining migration
-- history. Restore the control plane without enabling background borrowing or
-- replacing saved settings, revisions, cursors, or an active health lease.
INSERT INTO ticket_borrow_settings (id, running)
VALUES (1, false)
ON CONFLICT (id) DO NOTHING;

-- Migration 287 updated existing rows without changing the column defaults.
INSERT INTO ticket_borrow_selection (id, use_proxy_pool, rotate_identity)
VALUES (1, true, false)
ON CONFLICT (id) DO NOTHING;

INSERT INTO ticket_borrow_source_arbiter (id)
VALUES (1)
ON CONFLICT (id) DO NOTHING;

INSERT INTO ticket_borrow_candidate_pool_control (id)
VALUES (1)
ON CONFLICT (id) DO NOTHING;

INSERT INTO ticket_borrow_route_health (id)
VALUES (1)
ON CONFLICT (id) DO NOTHING;
