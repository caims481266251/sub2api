-- Borrowing history keeps a rolling 24-hour window, without a row-count cap.
-- The index makes periodic expiry cleanup independent of retained log volume.
CREATE INDEX ticket_borrow_events_created_at ON ticket_borrow_events(created_at);

-- Apply retention to existing history on upgrade, including paused systems.
DELETE FROM ticket_borrow_events WHERE created_at < NOW() - INTERVAL '24 hours';
