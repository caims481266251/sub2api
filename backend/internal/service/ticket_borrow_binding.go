package service

import (
	"context"
	"net/http"
)

type borrowBindingHint struct{ Session, Response string }
type borrowBindingKey struct{}

// Only hashes reach account selection. The original client binding survives
// gateway header rewriting and is also used at final route reservation.
func WithBorrowBusinessBinding(ctx context.Context, h http.Header, previous string) context.Context {
	return context.WithValue(ctx, borrowBindingKey{}, borrowBindingHint{borrowSessionKey(h), borrowResponseKey(previous)})
}

func (s *TicketBorrowService) borrowPreferredRoute(ctx context.Context, target int64, hint borrowBindingHint) (string, error) {
	preferred := ""
	for _, key := range []string{hint.Response, hint.Session} {
		if key == "" {
			continue
		}
		bound, found, err := s.repo.LookupSession(ctx, target, key)
		if err != nil {
			return "", ErrBorrowUnavailable
		}
		// An unknown response must never borrow an unrelated session's route.
		if key == hint.Response && !found {
			return "", ErrBorrowUnavailable
		}
		if found {
			if preferred != "" && preferred != bound {
				return "", ErrBorrowUnavailable
			}
			preferred = bound
		}
	}
	return preferred, nil
}
