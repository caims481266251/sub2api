package repository

// Scheduling expressions below share target t, account a and settings cfg.
// The target row serializes both claim paths; no new counter or policy knob is
// needed. Reserve a turn for route checks instead of racing the next candidate.
const borrowHealthInflight = `(SELECT count(*) FROM ticket_borrow_routes hr WHERE hr.target_id=t.account_id AND hr.health_token<>'' AND hr.health_until>clock_timestamp())`
const borrowProbeCapacity = `LEAST(CASE WHEN t.probe_policy_override THEN t.probe_concurrency ELSE cfg.probe_concurrency END,
 GREATEST(0,a.concurrency-(SELECT count(*) FROM ticket_borrow_route_leases WHERE target_id=t.account_id AND lease_until>clock_timestamp())))`
const borrowLastHealthTurn = `COALESCE((SELECT max(health_checked_at) FROM ticket_borrow_routes WHERE target_id=t.account_id),'-infinity'::timestamptz)`
const borrowLastCandidateTurn = `GREATEST(
 COALESCE((SELECT max(claimed_at) FROM ticket_borrow_candidate_pool WHERE target_id=t.account_id),'-infinity'::timestamptz),
 COALESCE((SELECT max(reserved_at) FROM ticket_borrow_requests WHERE target_id=t.account_id AND kind='probe' AND phase='mint' AND COALESCE(details->>'route_id','')=''),'-infinity'::timestamptz),
 COALESCE((SELECT max(reserved_at) FROM ticket_borrow_source_attempts WHERE target_id=t.account_id AND source_id IS NULL),'-infinity'::timestamptz))`

// A queued, valid candidate can use the other position while a slow quality
// check runs. With only one available position the two classes alternate.
const borrowCandidateWaiting = `cfg.running AND COALESCE(t.lease_until>clock_timestamp(),false) AND COALESCE(t.lease_settings_revision=cfg.revision,false) AND (
 (t.acquisition_identity='self') OR
 (EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool_demand pd WHERE pd.target_id=t.account_id AND pd.batch_id=t.lease_token AND pd.until_at>clock_timestamp())
 AND EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool p JOIN ticket_borrow_candidate_environments e ON e.id=p.id
 LEFT JOIN ticket_borrow_cookie_deadlines cd ON cd.cookie_hash=p.cookie_hash
 WHERE p.state='queued' AND p.lease_until>clock_timestamp() AND ` + borrowPoolCandidateValid + ` AND ` + borrowPoolSourceReady + `
 AND LEAST(p.admission_until,e.admission_until,COALESCE(cd.expires_at,p.admission_until))>clock_timestamp()+(45+cfg.continue_timeout_seconds+7)*INTERVAL '1 second')))`

const borrowHealthHasTurn = `(NOT (` + borrowCandidateWaiting + `) OR
 ((` + borrowProbeCapacity + `)>1 AND (` + borrowHealthInflight + `)<(` + borrowProbeCapacity + `)-1) OR
 ((` + borrowProbeCapacity + `)<=1 AND (` + borrowLastHealthTurn + `)<=(` + borrowLastCandidateTurn + `)))`

// Do not condition this on a free request budget: a due route also needs its
// next budget turn, otherwise a stream of two-shot candidates can starve a
// quality check indefinitely.
const borrowCandidateHasTurn = `(NOT EXISTS(SELECT 1 FROM ticket_borrow_routes r WHERE r.target_id=t.account_id AND ` + borrowHealthDue + `)
 OR (` + borrowHealthInflight + `)>0
 OR ((` + borrowProbeCapacity + `)<=1 AND (` + borrowLastHealthTurn + `)>(` + borrowLastCandidateTurn + `)))`

// A health job reserves its only request at acquisition. Candidates retain the
// remaining mint/continue budget across their existing renewable claim.
const borrowCandidateBudgetReserved = `(SELECT COALESCE(sum(GREATEST(0,2-
 (SELECT count(*) FROM ticket_borrow_requests pq WHERE pq.target_id=p.target_id AND pq.kind='probe' AND pq.details->>'candidate_id'=p.id))),0)
 FROM ticket_borrow_candidate_pool p WHERE p.target_id=$1 AND p.state='claimed' AND p.lease_until>clock_timestamp())`

// Use the same whole-job budget predicate when choosing a target and its
// route. An underfunded recovery on one target must not block other targets.
const borrowHealthBudgetAvailable = `(SELECT count(*) FROM ticket_borrow_requests WHERE target_id=$1 AND kind='probe' AND reserved_at>clock_timestamp()-INTERVAL '60 seconds')
 + (` + borrowCandidateBudgetReserved + `) + 1
 <=(SELECT GREATEST(1,COALESCE((recovery->>'probe_requests_per_minute')::int,12)) FROM ticket_borrow_settings WHERE id=1)`
