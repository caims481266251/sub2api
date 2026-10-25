package service

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// Fingerprint is deliberately distinct from stored authentication/session headers.
// Never copy a probe's complete headers into a business request.
func applyBorrowRouteFingerprint(headers http.Header, route *BorrowRoute, model string) (*tlsfingerprint.Profile, error) {
	if route == nil || headers == nil || ValidateBorrowFingerprint(route.Secret.Fingerprint) != nil {
		return nil, ErrBorrowFingerprintInvalid
	}
	f := route.Secret.Fingerprint
	f.Apply(headers, model)
	return f.Profile, nil
}

func cloneBorrowFingerprint(f *BorrowFingerprint) (*BorrowFingerprint, error) {
	if err := ValidateBorrowFingerprint(f); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	var snapshot BorrowFingerprint
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

type borrowWSFingerprintKey struct{}

func prepareBorrowWSDial(ctx context.Context, headers http.Header, route *BorrowRoute, model string) (context.Context, error) {
	if route == nil {
		return ctx, nil
	}
	profile, err := applyBorrowRouteFingerprint(headers, route, model)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, borrowWSFingerprintKey{}, profile), nil
}
