package service

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/tidwall/gjson"
)

type openAIInitialAdmissionKey struct{}

// Shared by nested forwards and protocol recovery within one account attempt.
// This records replay safety only; the handler owns selection and retry budgets.
func withOpenAIInitialAdmission(ctx context.Context) context.Context {
	if _, ok := ctx.Value(openAIInitialAdmissionKey{}).(*atomic.Bool); ok {
		return ctx
	}
	return context.WithValue(ctx, openAIInitialAdmissionKey{}, &atomic.Bool{})
}

func markOpenAIInitialAdmissionForContext(ctx context.Context, err error) error {
	attempted, _ := ctx.Value(openAIInitialAdmissionKey{}).(*atomic.Bool)
	if attempted == nil || attempted.Load() || agentIdentityTaskRecoveryWasTried(ctx) {
		return err
	}
	return markOpenAIInitialAdmissionError(err)
}

// Claim before handing work to any HTTP or WS transport. Even an inconclusive
// transport failure prevents a later local rejection from claiming no send.
func claimOpenAIInitialUpstreamAttempt(ctx context.Context) bool {
	attempted, _ := ctx.Value(openAIInitialAdmissionKey{}).(*atomic.Bool)
	return attempted != nil && !attempted.Swap(true) && !agentIdentityTaskRecoveryWasTried(ctx)
}

func markOpenAIBorrowPreSendRejection(err error, initialAttempt bool) error {
	var rejected *borrowPreSendError
	if initialAttempt && errors.As(err, &rejected) {
		return markOpenAIInitialAdmissionError(err)
	}
	return err
}

// OpenAIInitialAdmissionCanReselect checks original request semantics as well
// as continuation metadata retained after previous_response_id is normalized.
func OpenAIInitialAdmissionCanReselect(ctx context.Context, body []byte) bool {
	if borrowHTTPContinuationFromContext(ctx) != nil {
		return false
	}
	hint, _ := ctx.Value(borrowBindingKey{}).(borrowBindingHint)
	return hint.Response == "" && gjson.GetBytes(body, "previous_response_id").String() == "" &&
		!gjson.GetBytes(body, "conversation").Exists()
}
