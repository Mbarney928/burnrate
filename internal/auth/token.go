// Package auth issues and verifies API bearer tokens.
//
// Tokens are 256-bit random secrets. Only their SHA-256 hash is stored, so a leaked
// database does not leak usable credentials. A slow password hash (bcrypt/argon2) is not
// needed here because the secret has full entropy and cannot be brute-forced.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// Prefix marks burnrate tokens so they are easy to recognise in secret scanners and logs.
const Prefix = "brt_"

// secretBytes is the number of random bytes in a token (256 bits).
const secretBytes = 32

// ErrMalformed is returned for strings that cannot be burnrate tokens.
var ErrMalformed = errors.New("malformed token")

// NewToken returns a new random token and the hash to store for it.
func NewToken() (plain, hash string, err error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = Prefix + base64.RawURLEncoding.EncodeToString(buf)
	return plain, Hash(plain), nil
}

// Hash returns the hex SHA-256 of a token, which is what the database stores.
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// ParseBearer extracts and sanity-checks the token from an Authorization header value.
func ParseBearer(header string) (string, error) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", ErrMalformed
	}
	token = strings.TrimSpace(token)
	raw, found := strings.CutPrefix(token, Prefix)
	if !found {
		return "", ErrMalformed
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != secretBytes {
		return "", ErrMalformed
	}
	return token, nil
}
