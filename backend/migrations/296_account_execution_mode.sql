ALTER TABLE accounts ADD COLUMN execution_mode VARCHAR(20) NOT NULL DEFAULT 'normal';
ALTER TABLE accounts ADD CONSTRAINT accounts_execution_mode_check CHECK (execution_mode IN ('normal', 'vm_kernel'));
