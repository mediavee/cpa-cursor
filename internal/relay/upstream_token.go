package relay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mediavee/cpa-cursor/internal/cursor"
)

const (
	// renewMargin is how long before its expiry the account's token is renewed.
	renewMargin = 5 * time.Minute
	// minRenewGap keeps a persistent upstream 401 from turning into one exchange per request.
	minRenewGap = 5 * time.Second
	// exchangeTimeout bounds one exchange, whatever the caller that triggered it does.
	exchangeTimeout = 30 * time.Second
)

var errRenewTooSoon = errors.New("relay: the upstream token was renewed moments ago")

// tokenSource holds the account's access token, exchanged from its user API key.
type tokenSource struct {
	client *cursor.Client
	apiKey string
	now    func() time.Time

	mu  sync.Mutex
	tok string
	exp time.Time
	at  time.Time
}

// get returns a token valid for at least renewMargin, exchanging a new one when needed. A failed renewal
// keeps serving the current token until it really expires.
func (s *tokenSource) get(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != "" && s.now().Add(renewMargin).Before(s.exp) {
		return s.tok, nil
	}
	if err := s.exchangeLocked(ctx); err != nil {
		if s.tok != "" && s.now().Before(s.exp) {
			return s.tok, nil
		}
		return "", err
	}
	return s.tok, nil
}

// renew replaces stale, which Cursor just refused. It does nothing when another caller already replaced it.
func (s *tokenSource) renew(ctx context.Context, stale string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != stale {
		return nil
	}
	if s.now().Sub(s.at) < minRenewGap {
		return errRenewTooSoon
	}
	return s.exchangeLocked(ctx)
}

// held reports whether a token that has not expired is in memory.
func (s *tokenSource) held() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tok != "" && s.now().Before(s.exp)
}

func (s *tokenSource) exchangeLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exchangeTimeout)
	defer cancel()
	t, err := s.client.ExchangeAPIKey(ctx, s.apiKey)
	if err != nil {
		return fmt.Errorf("relay: renew the upstream token: %w", err)
	}
	exp, err := cursor.TokenExpiry(t.AccessToken)
	if err != nil {
		return fmt.Errorf("relay: the upstream token is unusable: %w", err)
	}
	s.tok, s.exp, s.at = t.AccessToken, exp, s.now()
	return nil
}
