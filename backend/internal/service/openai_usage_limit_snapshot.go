package service

import (
	"fmt"
	"math"
	"time"

	"github.com/tidwall/gjson"
)

const openAIUsageLimitSnapshotMaxAge = 5 * time.Minute

// Prefer the rejection's own reset. A usage-limit rejection can omit it, in
// which case a recent exhausted window from this credential supplies the end.
func openAI429BodyOrSnapshotResetAt(account *Account, body []byte, now time.Time) *time.Time {
	if resetUnix := parseOpenAIRateLimitResetTime(body); resetUnix != nil {
		resetAt := time.Unix(*resetUnix, 0)
		return &resetAt
	}
	return openAIUsageLimitSnapshotResetAt(account, body, now)
}

func openAIUsageLimitSnapshotResetAt(account *Account, body []byte, now time.Time) *time.Time {
	if !gjson.ValidBytes(body) || !isOpenAIOAuthAccount(account) || account.IsShadow() || account.IsSIWCEnabled() {
		return nil
	}
	errObj := gjson.GetBytes(body, "error")
	if !errObj.IsObject() {
		errObj = gjson.GetBytes(body, "response.error")
	}
	if errObj.Get("type").String() != "usage_limit_reached" && errObj.Get("code").String() != "usage_limit_reached" {
		return nil
	}
	updatedAt, err := parseTime(fmt.Sprint(account.Extra["codex_usage_updated_at"]))
	if err != nil || updatedAt.After(now) || now.Sub(updatedAt) >= openAIUsageLimitSnapshotMaxAge {
		return nil
	}
	var resetAt *time.Time
	for _, window := range []string{"5h", "7d"} {
		used := readOpenAIQuotaUsedPercent(account.Extra, window)
		if math.IsNaN(used) || math.IsInf(used, 0) || used < 100 {
			continue
		}
		// Relative reset metadata is anchored to its sample time by this helper.
		end, ok := openAICodexWindowResetAt(account.Extra, window)
		if ok && end.After(now) && (resetAt == nil || end.After(*resetAt)) {
			resetAt = &end
		}
	}
	return resetAt
}
