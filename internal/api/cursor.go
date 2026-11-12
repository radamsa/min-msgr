package api

import (
	cursorpkg "github.com/dav/min-msgr/internal/cursor"
)

const (
	cursorBackward = cursorpkg.Backward
	cursorForward  = cursorpkg.Forward
)

type cursor = cursorpkg.Cursor

func encodeCursor(seq int64, direction string) string {
	return cursorpkg.Encode(seq, direction)
}

func decodeCursor(value string) (cursor, error) {
	return cursorpkg.Decode(value)
}
