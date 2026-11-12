package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"strings"
)

const (
	// KeyPrefix makes API keys recognizable in logs and secret scanners.
	KeyPrefix = "sk_"
	keyLength = 32
	alphabet  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// Generate creates a new API key and its SHA-256 hash. The plaintext key must
// be shown to the user once and never stored.
func Generate() (plain string, hash []byte, err error) {
	raw := make([]byte, keyLength)
	max := big.NewInt(int64(len(alphabet)))
	for i := range raw {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", nil, fmt.Errorf("generate random key: %w", err)
		}
		raw[i] = alphabet[n.Int64()]
	}
	plain = KeyPrefix + string(raw)
	return plain, Hash(plain), nil
}

// Hash returns the SHA-256 digest of a plaintext API key.
func Hash(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}

// ValidFormat reports whether a bearer token looks like an API key. The
// database lookup is still required to authenticate it.
func ValidFormat(plain string) bool {
	return strings.HasPrefix(plain, KeyPrefix) && len(plain) > len(KeyPrefix)
}
