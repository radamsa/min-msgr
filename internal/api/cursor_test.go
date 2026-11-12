package api

import (
	"encoding/base64"
	"testing"
)

func TestCursorRoundTrip(t *testing.T) {
	for _, direction := range []string{cursorBackward, cursorForward} {
		encoded := encodeCursor(42, direction)
		decoded, err := decodeCursor(encoded)
		if err != nil {
			t.Fatalf("decodeCursor() error = %v", err)
		}
		if decoded.Seq != 42 || decoded.Direction != direction {
			t.Errorf("decoded = %+v, want seq 42 direction %q", decoded, direction)
		}
	}
}

func TestDecodeCursorRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "invalid base64", value: "!!!"},
		{name: "invalid json", value: base64.RawURLEncoding.EncodeToString([]byte("{"))},
		{name: "zero seq", value: base64.RawURLEncoding.EncodeToString([]byte(`{"s":0,"d":"b"}`))},
		{name: "bad direction", value: base64.RawURLEncoding.EncodeToString([]byte(`{"s":1,"d":"x"}`))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeCursor(tt.value); err == nil {
				t.Fatal("decodeCursor() error = nil, want error")
			}
		})
	}
}
