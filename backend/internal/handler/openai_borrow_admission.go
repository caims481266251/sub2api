package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func openAIAdmissionClientError(err error) (int, string, string) {
	if service.IsBorrowRouteUnavailable(err) {
		return http.StatusTooManyRequests, "rate_limit_error", "No verified, unexpired gateway route is available for this account and transport"
	}
	return http.StatusServiceUnavailable, "admission_unavailable", "Account eligibility changed; please retry with complete context"
}
