package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	plain, hash, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !strings.HasPrefix(plain, KeyPrefix) {
		t.Errorf("key %q does not start with %q", plain, KeyPrefix)
	}
	if len(plain) != len(KeyPrefix)+keyLength {
		t.Errorf("key length = %d, want %d", len(plain), len(KeyPrefix)+keyLength)
	}
	if len(hash) != 32 {
		t.Errorf("hash length = %d, want 32", len(hash))
	}
	if !bytes.Equal(hash, Hash(plain)) {
		t.Errorf("Generate() hash does not match Hash(plain)")
	}
}

func TestGenerateIsRandom(t *testing.T) {
	first, _, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two generated keys are identical")
	}
}

func TestValidFormat(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{KeyPrefix + "abc", true},
		{"sk_", false},
		{"", false},
		{"bearer_abc", false},
	}
	for _, tt := range tests {
		if got := ValidFormat(tt.key); got != tt.want {
			t.Errorf("ValidFormat(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}
