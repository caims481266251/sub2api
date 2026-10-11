package handler

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func openAIAdmissionClientError(err error) (int, string, string) {
	if errors.Is(err, service.ErrBorrowResponseRejected) {
		return http.StatusBadGateway, "api_error", "The upstream route changed after the request was sent. Please retry with complete context."
	}
	if service.IsBorrowRouteUnavailable(err) {
		return http.StatusServiceUnavailable, "api_error", "Upstream routing is temporarily unavailable. Please retry later."
	}
	var admission *service.OpenAITurnAdmissionError
	if errors.As(err, &admission) && admission.Reason == "latest_state_unavailable" {
		return http.StatusServiceUnavailable, "admission_unavailable", "Account eligibility could not be verified. Please retry later."
	}
	return http.StatusServiceUnavailable, "admission_unavailable", "Account eligibility changed; please retry with complete context"
}
