// Package sessions hosts many WhatsApp sessions in one process. Each session
// is one linked phone with its own bearer token and its own data directory
// (session.db, cache.db, media/), so nothing a session stores is reachable
// through another session's token.
//
// The token itself is never persisted: the registry keeps only its SHA-256.
// Tokens are 256 random bits, so a fast hash is sufficient — there is no
// low-entropy secret for a slow KDF to protect — and a lookup by hash needs
// no constant-time comparison, since the attacker controls the hash input,
// not the bytes being compared.
package sessions

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
)

// tokenBytes is the entropy of a session token.
const tokenBytes = 32

// tokenRe is the exact shape NewToken produces: 32 bytes, base64url, no
// padding. Anything else is rejected before it is hashed or looked up.
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// TokenHash is the SHA-256 of a session token: the only form of it stored.
type TokenHash [sha256.Size]byte

// NewToken returns a fresh random session token.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("sessions: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the stored form of token.
func HashToken(token string) TokenHash {
	return sha256.Sum256([]byte(token))
}

// ValidTokenShape reports whether token could have come from NewToken.
func ValidTokenShape(token string) bool {
	return tokenRe.MatchString(token)
}
