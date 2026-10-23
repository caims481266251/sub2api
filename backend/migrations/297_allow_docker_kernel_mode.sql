-- Schema-only capability update. Existing account rows are intentionally unchanged.
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_execution_mode_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_execution_mode_check CHECK (execution_mode IN ('normal', 'vm_kernel', 'docker_kernel'));
