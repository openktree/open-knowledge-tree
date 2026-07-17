// Package configs bundles the default configuration shipped with
// the binary. The embedded config.default.yaml is the last-resort
// fallback used by internal/config.Load when no on-disk
// config.default.yaml is found in any search path — the first-run
// auto-write step (see config.Load) then writes these bytes to
// <binary_dir>/configs/config.default.yaml so an operator gets an
// editable file without having to copy one out by hand.
//
// This mirrors backend/schema.go's MigrationsFS pattern: a single
// embed.FS rooted at this package, consumed by the config loader.
package configs

import "embed"

// DefaultConfigFS is an embed.FS rooted at this package. The
// default config lives at "config.default.yaml" inside it; callers
// read it via configs.DefaultConfigFS.ReadFile("config.default.yaml").
//
//go:embed config.default.yaml
var DefaultConfigFS embed.FS

// DefaultConfigBytes returns the embedded config.default.yaml
// content. It panics only if the embed directive fails to compile
// (which is a build-time, not a runtime, concern), so callers can
// treat the return as always-succeeding.
func DefaultConfigBytes() ([]byte, error) {
	return DefaultConfigFS.ReadFile("config.default.yaml")
}