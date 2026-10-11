package admin

import (
	"errors"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) borrowService(c *gin.Context) *service.TicketBorrowService {
	if h.accountTestService == nil || h.accountTestService.TicketBorrow == nil {
		response.Error(c, 503, "borrow_service_unavailable")
		return nil
	}
	return h.accountTestService.TicketBorrow
}
func borrowError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrBorrowRotationRetired) {
		response.ErrorWithDetails(c, 410, "Every acquisition now allocates a fresh environment", err.Error(), nil)
		return
	}
	if errors.Is(err, service.ErrBorrowRotationInvalid) {
		response.ErrorWithDetails(c, 400, "Invalid rotation selection or interval", err.Error(), nil)
		return
	}
	if errors.Is(err, service.ErrBorrowRotationPending) || errors.Is(err, service.ErrBorrowRotationNoProxy) || errors.Is(err, service.ErrBorrowRotationNoFingerprint) || errors.Is(err, service.ErrBorrowPaused) {
		response.ErrorWithDetails(c, 409, err.Error(), err.Error(), nil)
		return
	}
	if errors.Is(err, service.ErrBorrowConflict) {
		response.ErrorWithDetails(c, 409, "Configuration changed or source is busy; reload and retry", "borrow_configuration_conflict", nil)
		return
	}
	switch err.Error() {
	case "borrow_acquisition_identity_invalid", "borrow_admission_window_invalid", "borrow_event_query_invalid", "borrow_shadow_unsupported", "borrow_probe_policy_invalid", "borrow_selection_invalid", "borrow_restore_multiple_proxies", "borrow_source_name_invalid", "borrow_source_model_invalid", "borrow_source_credentials_required", "borrow_proxy_limit", "borrow_proxy_invalid", "borrow_settings_invalid", "borrow_target_unsupported", "borrow_legacy_config_conflict", "borrow_source_is_normal_account":
		response.BadRequest(c, err.Error())
	default:
		response.ErrorWithDetails(c, 503, "Borrowing operation failed; check configuration and credential encryption", "borrow_operation_failed", nil)
	}
}
func borrowID(c *gin.Context, key string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(key), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid_id")
		return 0, false
	}
	return id, true
}
func (h *AccountHandler) BorrowSources(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	items, err := s.Sources(c.Request.Context())
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, items)
}
func (h *AccountHandler) BorrowTransfer(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	var input struct {
		AccountID int64 `json:"account_id"`
		Confirmed bool  `json:"confirmed"`
	}
	if c.ShouldBindJSON(&input) != nil || input.AccountID <= 0 || !input.Confirmed {
		response.BadRequest(c, "explicit_transfer_required")
		return
	}
	item, err := s.TransferSource(c.Request.Context(), input.AccountID)
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, item)
}
func (h *AccountHandler) BorrowSaveSource(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	var id int64
	if c.Param("sourceID") != "" {
		var ok bool
		id, ok = borrowID(c, "sourceID")
		if !ok {
			return
		}
	}
	var input service.BorrowSourceInput
	if c.ShouldBindJSON(&input) != nil {
		response.BadRequest(c, "invalid_request")
		return
	}
	item, err := s.SaveSource(c.Request.Context(), id, input)
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, item)
}
func (h *AccountHandler) BorrowDeleteSource(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	id, ok := borrowID(c, "sourceID")
	if !ok {
		return
	}
	revision, err := strconv.ParseInt(c.Query("revision"), 10, 64)
	if err != nil || revision <= 0 {
		response.BadRequest(c, "revision_required")
		return
	}
	if err = s.DeleteSource(c.Request.Context(), id, revision); err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}
func (h *AccountHandler) BorrowSettings(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	if c.Request.Method == "PUT" {
		var input service.BorrowSettings
		if c.ShouldBindJSON(&input) != nil {
			response.BadRequest(c, "invalid_request")
			return
		}
		if err := s.SaveSettings(c.Request.Context(), input); err != nil {
			borrowError(c, err)
			return
		}
	}
	if c.Request.Method == "PATCH" {
		var input service.BorrowControls
		if c.ShouldBindJSON(&input) != nil {
			response.BadRequest(c, "invalid_request")
			return
		}
		if err := s.SaveControls(c.Request.Context(), input); err != nil {
			borrowError(c, err)
			return
		}
	}
	v, err := s.Settings(c.Request.Context())
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, v)
}
func (h *AccountHandler) BorrowTargets(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	items, err := s.Targets(c.Request.Context(), c.Query("include_disabled") == "true")
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, items)
}
func (h *AccountHandler) BorrowTarget(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	id, ok := borrowID(c, "id")
	if !ok {
		return
	}
	if c.Request.Method == "PUT" {
		var input struct {
			Enabled             *bool   `json:"enabled"`
			Revision            *int64  `json:"revision"`
			AcquisitionIdentity *string `json:"acquisition_identity"`
			AcquisitionRevision *int64  `json:"acquisition_revision"`
		}
		if c.ShouldBindJSON(&input) != nil || input.Enabled == nil || input.Revision == nil {
			response.BadRequest(c, "enabled_and_revision_required")
			return
		}
		var err error
		if input.AcquisitionIdentity != nil {
			if input.AcquisitionRevision == nil {
				response.BadRequest(c, "acquisition_revision_required")
				return
			}
			err = s.SetTargetIdentity(c.Request.Context(), id, *input.Enabled, *input.Revision, *input.AcquisitionRevision, *input.AcquisitionIdentity)
		} else {
			err = s.SetTarget(c.Request.Context(), id, *input.Enabled, *input.Revision)
		}
		if err != nil {
			borrowError(c, err)
			return
		}
	}
	item, err := s.Status(c.Request.Context(), id)
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, item)
}
func (h *AccountHandler) BorrowRefresh(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	id, ok := borrowID(c, "id")
	if !ok {
		return
	}
	if err := s.Refresh(c.Request.Context(), id); err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, gin.H{"queued": true})
}
func (h *AccountHandler) BorrowEvents(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	q := service.BorrowEventQuery{Stage: c.Query("stage"), Reason: c.Query("reason"), CandidateID: c.Query("candidate_id"), RouteID: c.Query("route_id"), Search: c.Query("search"), HideRotations: c.Query("hide_rotations") == "true"}
	for key, dest := range map[string]*int64{"before": &q.Before, "after": &q.After, "target_id": &q.TargetID, "source_account_id": &q.SourceAccount, "source_id": &q.SourceID} {
		if raw := c.Query(key); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				response.BadRequest(c, "borrow_event_query_invalid")
				return
			}
			*dest = n
		}
	}
	for key, dest := range map[string]*time.Time{"since": &q.Since, "until": &q.Until} {
		if raw := c.Query(key); raw != "" {
			value, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				response.BadRequest(c, "borrow_event_query_invalid")
				return
			}
			*dest = value
		}
	}
	items, err := s.QueryEvents(c.Request.Context(), q)
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, items)
}

func (h *AccountHandler) BorrowScan(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	id, ok := borrowID(c, "id")
	if !ok {
		return
	}
	if err := s.Scan(c.Request.Context(), id, c.Request.Method == "DELETE"); err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, gin.H{"queued": c.Request.Method != "DELETE"})
}

func (h *AccountHandler) BorrowTransferPreview(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	id, ok := borrowID(c, "id")
	if !ok {
		return
	}
	v, e := s.TransferPreview(c.Request.Context(), id)
	if e != nil {
		borrowError(c, e)
		return
	}
	response.Success(c, v)
}

func (h *AccountHandler) BorrowSelection(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	if c.Request.Method == "PUT" {
		var v service.BorrowSelection
		if c.ShouldBindJSON(&v) != nil {
			response.BadRequest(c, "borrow_selection_invalid")
			return
		}
		if err := s.SaveSelection(c.Request.Context(), v); err != nil {
			borrowError(c, err)
			return
		}
	}
	v, err := s.Selection(c.Request.Context())
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, v)
}
func (h *AccountHandler) BorrowSourceOptions(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	v, e := s.SourceOptions(c.Request.Context())
	if e != nil {
		borrowError(c, e)
		return
	}
	response.Success(c, v)
}
func (h *AccountHandler) BorrowRestoreSources(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	var v struct {
		Confirmed bool `json:"confirmed"`
	}
	if c.ShouldBindJSON(&v) != nil || !v.Confirmed {
		response.BadRequest(c, "explicit_transfer_required")
		return
	}
	ids, e := s.RestoreSources(c.Request.Context())
	if e != nil {
		borrowError(c, e)
		return
	}
	response.Success(c, gin.H{"account_ids": ids, "restored": len(ids)})
}
func (h *AccountHandler) BorrowLegacySourceGone(c *gin.Context) {
	response.ErrorWithDetails(c, 410, "Manage source accounts in the normal accounts page and select them here", "borrow_sources_use_normal_accounts", nil)
}

func (h *AccountHandler) BorrowSaveProbePolicy(c *gin.Context) {
	if _, observer := service.ObserverGroupIDs(c.Request.Context()); observer {
		response.Forbidden(c, "Administrator access required")
		return
	}
	s := h.borrowService(c)
	if s == nil {
		return
	}
	id, ok := borrowID(c, "id")
	if !ok {
		return
	}
	var input struct {
		Revision           *int64 `json:"revision"`
		Concurrency        *int   `json:"concurrency"`
		RenewBeforeSeconds *int   `json:"renew_before_seconds"`
		UseGlobal          bool   `json:"use_global"`
	}
	if c.ShouldBindJSON(&input) != nil || input.Revision == nil || (!input.UseGlobal && input.Concurrency == nil) {
		response.BadRequest(c, "borrow_probe_policy_invalid")
		return
	}
	policy := service.BorrowProbePolicy{Revision: *input.Revision, UseGlobal: input.UseGlobal}
	if input.Concurrency != nil {
		policy.Concurrency = *input.Concurrency
	}
	if input.RenewBeforeSeconds != nil {
		policy.RenewBeforeSeconds = *input.RenewBeforeSeconds
	}
	if err := s.SaveProbePolicy(c.Request.Context(), id, policy); err != nil {
		borrowError(c, err)
		return
	}
	item, err := s.Status(c.Request.Context(), id)
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, item)
}

func (h *AccountHandler) BorrowStatistics(c *gin.Context) {
	s := h.borrowService(c)
	if s == nil {
		return
	}
	target, _ := strconv.ParseInt(c.Query("target_id"), 10, 64)
	minutes, _ := strconv.Atoi(c.DefaultQuery("minutes", "15"))
	stats, err := s.Statistics(c.Request.Context(), target, minutes)
	if err != nil {
		borrowError(c, err)
		return
	}
	response.Success(c, stats)
}
