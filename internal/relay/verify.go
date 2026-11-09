package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	verifyTimeout = 10 * time.Second
	// maxVerifyEntries bounds the cache: every distinct bearer, valid or not, gets an entry.
	maxVerifyEntries = 4096
)

// verifier asks the gateway whether a client key is valid and remembers the answer briefly.
type verifier struct {
	url         string
	client      *http.Client
	ttl, negTTL time.Duration
	now         func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]verdict
}

type verdict struct {
	valid   bool
	expires time.Time
}

func newVerifier(url string, ttl, negTTL time.Duration, now func() time.Time) *verifier {
	return &verifier{
		url: url, ttl: ttl, negTTL: negTTL, now: now,
		client: &http.Client{Timeout: verifyTimeout},
		cache:  make(map[[sha256.Size]byte]verdict),
	}
}

// valid answers from the cache or from the gateway: 200 is valid, any other 4xx is not. An unreachable
// gateway or a 5xx is an error and is not cached, so the caller fails closed without banning the key.
func (v *verifier) valid(ctx context.Context, key string) (bool, error) {
	id := sha256.Sum256([]byte(key))
	now := v.now()
	v.mu.Lock()
	if e, ok := v.cache[id]; ok && now.Before(e.expires) {
		v.mu.Unlock()
		return e.valid, nil
	}
	v.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := v.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("relay: verifier unreachable: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	var ok bool
	var ttl time.Duration
	switch {
	case resp.StatusCode == http.StatusOK:
		ok, ttl = true, v.ttl
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		ok, ttl = false, v.negTTL
	default:
		return false, fmt.Errorf("relay: verifier answered %d", resp.StatusCode)
	}
	v.store(id, verdict{valid: ok, expires: now.Add(ttl)})
	return ok, nil
}

func (v *verifier) store(id [sha256.Size]byte, e verdict) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.cache) >= maxVerifyEntries {
		now := v.now()
		for k, old := range v.cache {
			if !now.Before(old.expires) {
				delete(v.cache, k)
			}
		}
		if len(v.cache) >= maxVerifyEntries {
			return
		}
	}
	v.cache[id] = e
}

// keyTag is the short hash of a client key that logs carry in its place.
func keyTag(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}
