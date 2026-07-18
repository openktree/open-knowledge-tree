package registry

import (
	"encoding/base64"
	"fmt"
	"strconv"
)

// encodeBase64Int encodes an integer as a base64 string for use as
// an opaque pagination cursor. The cursor stays an implementation
// detail of the registry search provider — the caller treats it as
// opaque and passes it back as next_cursor.
func encodeBase64Int(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(n)))
}

// decodeBase64Int is the inverse of encodeBase64Int. A non-base64
// or non-integer input returns an error so the caller can fall back
// to the first page.
func decodeBase64Int(s string) (int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("registry: decoding cursor: %w", err)
	}
	n, err := strconv.Atoi(string(b))
	if err != nil {
		return 0, fmt.Errorf("registry: parsing cursor offset: %w", err)
	}
	return n, nil
}