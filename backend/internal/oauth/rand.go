package oauth

import "crypto/rand"

// readRand is a tiny wrapper around crypto/rand.Read so tests can
// replace it if they ever need deterministic tokens (they don't
// today, but the indirection keeps the package testable). It is
// intentionally not exported.
func readRand(b []byte) (int, error) {
	return rand.Read(b)
}