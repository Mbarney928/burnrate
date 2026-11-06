package auth

import (
	"strings"
	"testing"
)

func TestNewTokenRoundTrip(t *testing.T) {
	plain, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, Prefix) || len(hash) != 64 || hash != Hash(plain) {
		t.Fatalf("unexpected token/hash shape: %d chars, hash %d chars", len(plain), len(hash))
	}
	if strings.Contains(hash, plain) {
		t.Fatal("hash must not contain the secret")
	}
	got, err := ParseBearer("Bearer " + plain)
	if err != nil || got != plain {
		t.Fatalf("ParseBearer = %q, %v", got, err)
	}
	if got, err := ParseBearer("bearer  " + plain); err != nil || got != plain {
		t.Fatalf("scheme should be case-insensitive: %q, %v", got, err)
	}
}

func TestTokensAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		plain, _, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[plain] {
			t.Fatal("duplicate token generated")
		}
		seen[plain] = true
	}
}

func TestParseBearerRejectsMalformed(t *testing.T) {
	plain, _, _ := NewToken()
	for _, h := range []string{
		"",
		plain,
		"Basic " + plain,
		"Bearer",
		"Bearer abc",
		"Bearer " + Prefix + "not*base64",
		"Bearer " + Prefix + "c2hvcnQ",
		"Bearer " + strings.TrimPrefix(plain, Prefix),
	} {
		if _, err := ParseBearer(h); err == nil {
			t.Errorf("ParseBearer(%q) should fail", h)
		}
	}
}
