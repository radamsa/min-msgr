// Package cursor encodes opaque message pagination cursors. Clients treat the
// value as a black box; both the REST API and the web UI use the same format.
package cursor

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

const (
	// Backward selects messages older than the cursor position.
	Backward = "b"
	// Forward selects messages newer than the cursor position.
	Forward = "f"
)

// Cursor is a message position and pagination direction.
type Cursor struct {
	Seq       int64  `json:"s"`
	Direction string `json:"d"`
}

// Encode returns an opaque cursor string.
func Encode(seq int64, direction string) string {
	payload, _ := json.Marshal(Cursor{Seq: seq, Direction: direction})
	return base64.RawURLEncoding.EncodeToString(payload)
}

// Decode parses and validates an opaque cursor string.
func Decode(value string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return Cursor{}, fmt.Errorf("invalid cursor encoding: %w", err)
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, fmt.Errorf("invalid cursor payload: %w", err)
	}
	if c.Seq <= 0 {
		return Cursor{}, fmt.Errorf("invalid cursor position")
	}
	if c.Direction != Backward && c.Direction != Forward {
		return Cursor{}, fmt.Errorf("invalid cursor direction")
	}
	return c, nil
}
