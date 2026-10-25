package service

import (
	"time"
)

// No credentials or proxy URLs: lifecycle events are safe to export with the
// ordinary candidate evidence stream.
type BorrowReplenishmentAudit struct {
	EnvironmentVersion int64      `json:"environment_version,omitempty"`
	Until              *time.Time `json:"until,omitempty"`
	Inventory          int        `json:"inventory,omitempty"`
	ExitReason         string     `json:"exit_reason,omitempty"`
}

func borrowSubjectHash(a *Account) string {
	return borrowHash(borrowCredential(a.Credentials, "chatgpt_account_id") + ":" + borrowCredential(a.Credentials, "chatgpt_user_id"))
}
