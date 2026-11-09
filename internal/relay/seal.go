package relay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mediavee/cpa-cursor/internal/cursor"
)

const sealInfo = "cursor-relay client token v1"

var errBadToken = errors.New("relay: not a token of this relay")

// sealer mints and opens the tokens the relay hands to its clients. A token keeps the upstream token's JWT
// header and claims, which clients read; its signature part is the client key sealed with the relay's key, so
// the relay keeps no state and a token is worthless to Cursor.
type sealer struct{ aead cipher.AEAD }

func newSealer(secret string) (*sealer, error) {
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, sealInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("relay: derive the seal key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("relay: seal cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("relay: seal cipher: %w", err)
	}
	return &sealer{aead: aead}, nil
}

// mint returns upstream's header and claims with clientKey sealed in place of the signature. The header and
// claims are the authenticated data, so a sealed key cannot be moved to other claims.
func (s *sealer) mint(upstream, clientKey string) (string, error) {
	parts := strings.Split(upstream, ".")
	if len(parts) != 3 {
		return "", errBadToken
	}
	signed := parts[0] + "." + parts[1]
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("relay: nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(clientKey), []byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// open returns the client key sealed in token and rejects a token that is not the relay's or has expired.
func (s *sealer) open(token string, now time.Time) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errBadToken
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sealed) < s.aead.NonceSize() {
		return "", errBadToken
	}
	nonce, ct := sealed[:s.aead.NonceSize()], sealed[s.aead.NonceSize():]
	key, err := s.aead.Open(nil, nonce, ct, []byte(parts[0]+"."+parts[1]))
	if err != nil {
		return "", errBadToken
	}
	exp, err := cursor.TokenExpiry(token)
	if err != nil || !now.Before(exp) {
		return "", errors.New("relay: token expired")
	}
	return string(key), nil
}
