package config

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Port     int            `mapstructure:"port"`
	Database DatabaseConfig `mapstructure:"database"`
	Storage  StorageConfig  `mapstructure:"storage"`
	S3       S3Config       `mapstructure:"s3"`
	Auth     AuthConfig     `mapstructure:"auth"`
	// Concurrency caps the number of in-flight heavy-memory
	// operations (push/pull decomposition, pull source). Each holds
	// the request body + a re-marshalled copy + struct amplification
	// (embeddings as map[string][]float32) — easily 80–100MB for a
	// 19MB payload. The semaphore bounds peak memory by queueing
	// requests beyond the cap instead of OOM-killing the VM.
	// 0 = unbounded (legacy behavior, not recommended).
	Concurrency ConcurrencyConfig `mapstructure:"concurrency"`
	// Promptset governs registry-side enforcement of the
	// promptset_hash tag on pushed decompositions. When validation
	// is enabled, a PushDecomposition with an empty promptset_hash
	// is rejected with 400 — the forcing function that guarantees
	// every decomposition on the registry carries a real
	// philosophy hash so pullers can filter by accepted promptset.
	// Default false: existing registries keep the legacy
	// accept-and-store-as-NULL behavior until an operator opts in.
	Promptset PromptsetConfig `mapstructure:"promptset"`
	// EmailValidation governs account email validation. When
	// enabled, Register issues a verification token (stored hashed
	// in email_verifications) and emails a link instead of an
	// immediate JWT; Login refuses unverified accounts. Off by
	// default so existing registries keep the legacy auto-login
	// behavior. Mirrors the Promptset opt-in toggle shape.
	EmailValidation EmailValidationConfig `mapstructure:"email_validation"`
	// GraphUpload configures the admin file-upload path for graph
	// bundles (POST /api/v1/admin/graphs/upload + the /ui/graphs/upload
	// form). The upload spools the multipart file to a temp file on
	// TempDir before streaming to S3, so TempDir must be on a volume
	// large enough to hold the largest expected bundle (the Fly
	// registry_data mount at /data is the natural target). MaxSizeBytes
	// is a backstop against runaway uploads; 0 = unlimited (the temp
	// file + disk space are the real bounds).
	GraphUpload GraphUploadConfig `mapstructure:"graph_upload"`
}

// GraphUploadConfig configures the admin file-upload path. Bundles can
// be 100s of GB (large repos with embedded PDFs/images), so the upload
// path is built around a temp-file spool + streaming S3 multipart —
// peak memory is one multipart part (64 MB) regardless of bundle size.
type GraphUploadConfig struct {
	// MaxSizeBytes rejects uploads larger than this many bytes. 0 =
	// unlimited (default). The HTTP layer enforces it via
	// http.MaxBytesReader (early reject for clients that announce
	// Content-Length) and the service re-checks after the temp-file
	// spool (the real guard for chunked uploads).
	MaxSizeBytes int64 `mapstructure:"max_size_bytes"`
	// TempDir is where the upload spools the incoming bundle before
	// streaming to S3. Empty = OS default (typically /tmp). On Fly,
	// operators point this at the mounted volume (/data/tmp) so a
	// multi-GB upload doesn't fill the container's writable layer.
	TempDir string `mapstructure:"temp_dir"`
}

// EmailValidationConfig configures account email validation. KISS:
// one toggle, one SMTP block, one TTL. When EnableValidation is
// false (the default) Register/Login keep their legacy behavior
// (immediate JWT, no verification step) and the email_verifications
// table is simply unused. When true, Register mints a random
// token (auth.GenerateAPIToken) + hashes it (auth.HashToken),
// stores the hash, and emails a link to <PublicBaseURL>/ui/verify-email?token=…;
// the user must click before Login accepts them.
type EmailValidationConfig struct {
	// EnableValidation turns on the verify-before-login flow.
	// Default false. Mirrors promptset.enable_validation.
	EnableValidation bool `mapstructure:"enable_validation"`
	// FromAddress is the envelope sender used in the verification
	// email. Optional; defaults to "no-reply@localhost" (fine for
	// dev; operators set a real address in production).
	FromAddress string `mapstructure:"from_address"`
	// PublicBaseURL is the base the verification link is built
	// against (e.g. "https://registry.example.com"). Required
	// when EnableValidation is true so the link resolves to a
	// real route. Trailing slash is trimmed.
	PublicBaseURL string `mapstructure:"public_base_url"`
	// TokenTTL is how long a verification token lives. Default 24h.
	TokenTTL time.Duration `mapstructure:"token_ttl"`
	// ResendCooldown bounds how fast a user can request a new
	// verification email via /api/v1/auth/resend-verification.
	// Default 60s. The handler looks at the most recent
	// email_verifications row's created_at; a request within the
	// cooldown is a no-op (still returns 200, no email sent).
	ResendCooldown time.Duration `mapstructure:"resend_cooldown"`
	// SMTP is the outbound mail config. When Host is empty the
	// mailer falls back to NoopMailer (logs the verification URL
	// to stdout instead of sending) — the dev path so a local
	// registry boots without an SMTP server.
	SMTP SMTPConfig `mapstructure:"smtp"`
}

// SMTPConfig is the outbound SMTP envelope. Stdlib net/smtp only;
// no third-party mailer dependency, no SES/SendGrid adapter (KISS).
type SMTPConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	TLS      bool   `mapstructure:"tls"`
}

// PromptsetConfig configures registry-side promptset_hash
// enforcement. The hash itself is produced by the contributing OKT
// backend (promptset.RegistryHashPromptset over the 4 shared
// phases); the registry only validates and indexes it.
type PromptsetConfig struct {
	// EnableValidation rejects PushDecomposition calls that omit
	// promptset_hash (400 Bad Request). Default false. Enabling
	// this requires every contributing OKT backend to have a
	// configured promptset resolver (the default built-in promptset
	// counts). Pre-existing decomposition rows with NULL
	// promptset_hash stay pullable: the pull-side filter treats an
	// empty hash as always-accepted (legacy behavior).
	EnableValidation bool `mapstructure:"enable_validation"`
}

type ConcurrencyConfig struct {
	Push int `mapstructure:"push"` // push decomposition slots (default 8)
	Pull int `mapstructure:"pull"` // pull decomposition / source slots (default 8)
}

type AuthConfig struct {
	JWTSecret       string        `mapstructure:"jwt_secret"`
	TokenTTL        time.Duration `mapstructure:"token_ttl"`
	AuthMode        string        `mapstructure:"auth_mode"` // "open" | "read-open" | "closed"
	BootstrapAdmins []string      `mapstructure:"bootstrap_admins"`
}

type DatabaseConfig struct {
	Driver string `mapstructure:"driver"` // "sqlite" or "postgres"
	URL    string `mapstructure:"url"`
}

type StorageConfig struct {
	Backend        string `mapstructure:"backend"` // "s3" or "filesystem"
	FilesystemRoot string `mapstructure:"filesystem_root"`
}

type S3Config struct {
	Endpoint       string `mapstructure:"endpoint"`
	Region         string `mapstructure:"region"`
	Bucket         string `mapstructure:"bucket"`
	AccessKey      string `mapstructure:"access_key"`
	SecretKey      string `mapstructure:"secret_key"`
	PathStyle      bool   `mapstructure:"path_style"`
	PresignTTL     int    `mapstructure:"presign_ttl"`      // seconds
	PresignBaseURL string `mapstructure:"presign_base_url"` // public-facing URL for presigned URLs; empty = don't presign (dev)
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")

	v.SetDefault("port", 8080)
	v.SetDefault("database.driver", "sqlite")
	v.SetDefault("database.url", "registry.db")
	v.SetDefault("storage.backend", "s3")
	v.SetDefault("storage.filesystem_root", "/data/files")
	v.SetDefault("s3.endpoint", "http://localhost:9000")
	v.SetDefault("s3.region", "us-east-1")
	v.SetDefault("s3.bucket", "okt-registry")
	v.SetDefault("s3.path_style", true)
	v.SetDefault("s3.presign_ttl", 3600)
	v.SetDefault("s3.presign_base_url", "")
	v.SetDefault("s3.access_key", "")
	v.SetDefault("s3.secret_key", "")
	v.SetDefault("auth.jwt_secret", "change-me-in-production")
	v.SetDefault("auth.token_ttl", "24h")
	v.SetDefault("auth.auth_mode", "open")
	v.SetDefault("concurrency.push", 8)
	v.SetDefault("concurrency.pull", 8)
	v.SetDefault("promptset.enable_validation", false)

	// Email validation defaults: disabled (legacy auto-login), a
	// 24h token TTL, a 60s resend cooldown, and an empty SMTP
	// host (NoopMailer fallback so dev boots without a mail
	// server). Operators opt in via REGISTRY_EMAIL_VALIDATION_ENABLE_VALIDATION=true
	// and friends.
	v.SetDefault("email_validation.enable_validation", false)
	v.SetDefault("email_validation.from_address", "no-reply@localhost")
	v.SetDefault("email_validation.public_base_url", "")
	v.SetDefault("email_validation.token_ttl", "24h")
	v.SetDefault("email_validation.resend_cooldown", "60s")
	v.SetDefault("email_validation.smtp.host", "")
	v.SetDefault("email_validation.smtp.port", 587)
	v.SetDefault("email_validation.smtp.username", "")
	v.SetDefault("email_validation.smtp.password", "")
	v.SetDefault("email_validation.smtp.tls", false)

	// Graph upload defaults: unlimited size (the temp file + disk
	// space are the real bounds), OS-default temp dir. Operators
	// with large bundles set REGISTRY_GRAPH_UPLOAD_TEMP_DIR to the
	// mounted volume (e.g. /data/tmp on Fly) and optionally
	// REGISTRY_GRAPH_UPLOAD_MAX_SIZE_BYTES as a backstop.
	v.SetDefault("graph_upload.max_size_bytes", 0)
	v.SetDefault("graph_upload.temp_dir", "")

	v.SetEnvPrefix("REGISTRY")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("reading config: %w", err)
		}
		log.Printf("loaded config from %s", path)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}
	return &cfg, nil
}
