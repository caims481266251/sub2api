-- New targets inherit these defaults; existing explicit or non-default policies
-- remain account overrides. Admission lifetime is an independent global rule.
ALTER TABLE ticket_borrow_settings ADD COLUMN IF NOT EXISTS probe_concurrency
    INTEGER NOT NULL DEFAULT 2 CHECK (probe_concurrency BETWEEN 1 AND 32);
ALTER TABLE ticket_borrow_settings ADD COLUMN IF NOT EXISTS renew_before_seconds
    INTEGER NOT NULL DEFAULT 15 CHECK (renew_before_seconds BETWEEN 0 AND 240);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_schema=current_schema() AND table_name='ticket_borrow_targets'
                     AND column_name='probe_policy_override') THEN
        ALTER TABLE ticket_borrow_targets ADD COLUMN probe_policy_override BOOLEAN NOT NULL DEFAULT false;
        UPDATE ticket_borrow_targets SET probe_policy_override=true
         WHERE probe_policy_revision>1 OR probe_concurrency<>2 OR renew_before_seconds<>15;
    END IF;
END $$;
