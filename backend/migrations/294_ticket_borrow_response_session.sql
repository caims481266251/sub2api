-- Opaque effective session identity for responses produced after route expiry.
-- This is routing metadata only; no input, output or conversation history.
ALTER TABLE ticket_borrow_sessions ADD COLUMN effective_session_id TEXT NOT NULL DEFAULT '';
