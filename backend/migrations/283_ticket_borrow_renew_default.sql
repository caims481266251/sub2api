-- A 180-second renewal window leaves 60 seconds before the default 120-second
-- business admission cutoff. Preserve administrator-saved global/account values.
ALTER TABLE ticket_borrow_settings ALTER COLUMN renew_before_seconds SET DEFAULT 180;
ALTER TABLE ticket_borrow_targets ALTER COLUMN renew_before_seconds SET DEFAULT 180;

-- Earlier migrations seed the singleton with the old default. Only untouched
-- settings use this bootstrap upgrade; saved settings are changed through the API.
UPDATE ticket_borrow_settings SET renew_before_seconds=180,revision=revision+1
 WHERE revision=1 AND renew_before_seconds=15;
