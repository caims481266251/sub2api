package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIInitialAdmissionMarker(t *testing.T) {
	for _, reason := range []string{"borrow_route_unavailable", "account_binding_changed", "latest_state_unavailable", "account_ineligible", "model_ticket_unavailable", "group_membership_changed"} {
		t.Run(reason, func(t *testing.T) {
			original := denyOpenAITurn(reason)
			marked := markOpenAIInitialAdmissionError(original)
			require.False(t, IsOpenAIInitialAdmissionRejection(original), "later admission errors must remain non-retryable")
			require.Equal(t, reason == "account_binding_changed" || reason == "borrow_route_unavailable", IsOpenAIInitialAdmissionRejection(fmt.Errorf("wrapped: %w", marked)))
			require.True(t, IsOpenAITurnAdmissionError(marked))
		})
	}
}

func TestOpenAIInitialAdmissionRecoveryDoesNotMarkReplaySafe(t *testing.T) {
	selected := ticketTestAccount(100)
	latest := *selected
	latest.Extra = map[string]any{"openai_excel_bps": true}
	svc := &OpenAIGatewayService{accountRepo: &turnAdmissionRepo{account: &latest}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, err := svc.forwardAsChatCompletions(markAgentIdentityTaskRecoveryTried(context.Background()), c, selected,
		[]byte(`{"model":"gpt-6-astra","messages":[]}`), "", "", false)
	require.True(t, IsOpenAITurnAdmissionError(err))
	require.False(t, IsOpenAIInitialAdmissionRejection(err), "task recovery has already sent an upstream request")
	c.Request.URL.Path = "/v1/messages"
	_, err = svc.ForwardAsAnthropic(markAgentIdentityTaskRecoveryTried(context.Background()), c, selected,
		[]byte(`{"model":"gpt-6-astra","messages":[]}`), "", "")
	require.True(t, IsOpenAITurnAdmissionError(err))
	require.False(t, IsOpenAIInitialAdmissionRejection(err), "Messages recovery must use the same replay guard")
}

func TestOpenAIInitialAdmissionTracksTheWholeForward(t *testing.T) {
	ctx := withOpenAIInitialAdmission(t.Context())
	first := claimOpenAIInitialUpstreamAttempt(ctx)
	rejection := borrowPreSendRejection(ErrBorrowUnavailable)
	require.True(t, IsOpenAIInitialAdmissionRejection(markOpenAIBorrowPreSendRejection(rejection, first)))
	// Nested protocol or same-account recovery inherits the original attempt.
	ctx = withOpenAIInitialAdmission(ctx)
	require.False(t, IsOpenAIInitialAdmissionRejection(markOpenAIInitialAdmissionForContext(ctx, ErrBorrowUnavailable)))
	second := claimOpenAIInitialUpstreamAttempt(ctx)
	require.False(t, IsOpenAIInitialAdmissionRejection(markOpenAIBorrowPreSendRejection(rejection, second)))
	require.False(t, IsOpenAIInitialAdmissionRejection(markOpenAIBorrowPreSendRejection(ErrBorrowUnavailable, true)), "unproven transport errors cannot authorize replay")
	dbErr := errors.New("database unavailable")
	denied := markOpenAIBorrowPreSendRejection(borrowPreSendRejection(dbErr), true)
	require.ErrorIs(t, denied, dbErr)
	require.True(t, IsOpenAITurnAdmissionError(denied))
	require.False(t, IsBorrowRouteUnavailable(denied))
	require.False(t, IsOpenAIInitialAdmissionRejection(denied))
}

func TestOpenAIInitialAdmissionPreservesNormalizedContinuationBinding(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":"complete history"}`)
	require.True(t, OpenAIInitialAdmissionCanReselect(t.Context(), body))
	ctx := context.WithValue(t.Context(), borrowHTTPContinuationKey{}, &borrowHTTPContinuation{TargetID: 11, Response: "previous"})
	require.False(t, OpenAIInitialAdmissionCanReselect(ctx, body), "normalizing away previous_response_id does not unbind the account")
	ctx = WithBorrowBusinessBinding(t.Context(), http.Header{}, "resp_previous")
	require.False(t, OpenAIInitialAdmissionCanReselect(ctx, body))
}
