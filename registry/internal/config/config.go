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
}

type AuthConfig struct {
	JWTSecret      string        `mapstructure:"jwt_secret"`
	TokenTTL       time.Duration `mapstructure:"token_ttl"`
	AuthMode       string        `mapstructure:"auth_mode"` // "open" | "read-open" | "closed"
	BootstrapAdmins []string     `mapstructure:"bootstrap_admins"`
}

type DatabaseConfig struct {
	Driver string `mapstructure:"driver"` // "sqlite" or "postgres"
	URL    string `mapstructure:"url"`
}

type StorageConfig struct {
	Backend string `mapstructure:"backend"` // "s3" or "filesystem"
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
