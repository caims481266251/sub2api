package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Account row lock serializes concurrent provider 401s across app replicas.
// Compare the rejected credential snapshot before changing state: an old
// in-flight response must never revoke freshly replaced credentials.
func (r *openAIOAuthReauthRepository) QueueContainerReauth(ctx context.Context, snapshot *service.Account, dispatchedHash string, manual bool) (bool, error) {
	expected, err := json.Marshal(normalizeJSONMap(snapshot.Credentials))
	if err != nil {
		return true, err
	}
	digest := sha256.Sum256(expected)
	hash := hex.EncodeToString(digest[:])
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	var storedCredentials []byte
	err = tx.QueryRowContext(ctx, `SELECT credentials FROM accounts
 WHERE id=$1 AND deleted_at IS NULL AND execution_mode='docker_kernel'
 AND platform='openai' AND type='oauth' AND parent_account_id IS NULL FOR NO KEY UPDATE`, snapshot.ID).Scan(&storedCredentials)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	var current map[string]any
	if err := json.Unmarshal(storedCredentials, &current); err != nil {
		return true, err
	}
	currentJSON, err := json.Marshal(normalizeJSONMap(current))
	if err != nil {
		return true, err
	}
	currentDigest := sha256.Sum256(currentJSON)
	if dispatchedHash != "" {
		hash = dispatchedHash
	}
	if hex.EncodeToString(currentDigest[:]) != hash {
		return true, nil
	}
	var configured bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM openai_oauth_reauth_configs
 WHERE account_id=$1 AND credential_mode='password_totp'
 AND COALESCE(password_ciphertext,'')<>'' AND COALESCE(totp_secret_ciphertext,'')<>'')`, snapshot.ID).Scan(&configured)
	if err != nil {
		return true, err
	}
	// Missing saved login details still uses the versioned auth pause; never
	// fall back to SetError, which overwrites the manual scheduling switch.
	// A failed task for these credentials is terminal for automatic attempts.
	// Explicit manual reauthorization can always create a new task.
	var alreadyAttempted bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM openai_oauth_reauth_tasks
 WHERE account_id=$1 AND (status IN ('queued','running','callback_processing')
 OR (NOT $3 AND oauth_profile='codex' AND status='failed' AND expected_credentials_hash=$2
 AND NOT EXISTS (SELECT 1 FROM openai_totp_rotations rotation WHERE rotation.task_id=openai_oauth_reauth_tasks.id))))`, snapshot.ID, hash, manual).Scan(&alreadyAttempted)
	if err != nil {
		return true, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET status=CASE WHEN status='active' THEN 'error' ELSE status END,
 error_message=CASE WHEN status='active' OR error_message=$2
 OR (status='error' AND error_message LIKE ANY(ARRAY['Token revoked (401):%', 'Unauthorized (401):%', 'Authentication failed (401):%', 'OAuth 401 (no refresh_token):%']))
 THEN $2 ELSE error_message END,
 updated_at=NOW() WHERE id=$1`, snapshot.ID, service.OpenAIContainerAuthRequired)
	if err != nil {
		return true, err
	}
	if configured && !alreadyAttempted {
		// The existing unique active-task index also serializes manual triggers.
		_, err = tx.ExecContext(ctx, `INSERT INTO openai_oauth_reauth_tasks(account_id,status,stage,expected_credentials_hash)
 VALUES($1,'queued','queued',$2) ON CONFLICT DO NOTHING`, snapshot.ID, hash)
		if err != nil {
			return true, err
		}
	}
	if err = enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged, &snapshot.ID, nil, nil); err != nil {
		return true, err
	}
	return true, tx.Commit()
}

// Called only after both encrypted configuration and guard enrollment succeed.
// It cannot turn on a manual scheduling switch or clear another error.
func (r *openAIOAuthReauthRepository) CompleteContainerTwoFAEnrollment(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, `WITH updated AS (
 UPDATE accounts a SET status='active', error_message='', updated_at=NOW()
 WHERE a.id=$1 AND a.deleted_at IS NULL AND a.execution_mode='docker_kernel'
 AND a.status='error' AND a.error_message=$2
 AND EXISTS(SELECT 1 FROM openai_oauth_reauth_configs c JOIN account_token_guard_v2_accounts g USING(account_id)
 WHERE c.account_id=a.id AND c.credential_mode='password_totp'
 AND COALESCE(c.password_ciphertext,'')<>'' AND COALESCE(c.totp_secret_ciphertext,'')<>''
 AND lower(c.login_email)=lower(a.credentials->>'email')) RETURNING a.id)
 INSERT INTO scheduler_outbox(event_type,account_id) SELECT $3,id FROM updated`,
		id, service.OpenAIContainerTwoFAEnrollmentPending, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		// Another successful retry may already have finalized enrollment.
		var pending bool
		err = r.db.QueryRowContext(ctx, "SELECT error_message=$2 FROM accounts WHERE id=$1 AND deleted_at IS NULL", id, service.OpenAIContainerTwoFAEnrollmentPending).Scan(&pending)
		if err != nil || pending {
			return infraerrors.Conflict("OPENAI_REAUTH_ENROLLMENT_PENDING", "Login information is not fully saved; retry enrollment")
		}
	}
	return nil
}
