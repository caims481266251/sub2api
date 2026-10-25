-- Fair scheduling reads ownership history without scanning all prior tests.
CREATE INDEX ticket_borrow_requests_batch ON ticket_borrow_requests(batch_id);
CREATE INDEX ticket_borrow_requests_candidate_history
 ON ticket_borrow_requests(target_id,(details->>'candidate_id')) WHERE kind='probe';
CREATE INDEX ticket_borrow_candidate_claim_history
 ON ticket_borrow_candidate_pool(target_id,claimed_at DESC) WHERE claimed_at IS NOT NULL;
CREATE INDEX ticket_borrow_self_attempt_history
 ON ticket_borrow_source_attempts(target_id,reserved_at DESC) WHERE source_id IS NULL;
