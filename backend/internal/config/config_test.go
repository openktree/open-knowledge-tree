package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_DefaultsFromLegacyBlock verifies the backward-compat
// path: a config with only the legacy `database:` block is
// rewritten to `databases.default`, and a missing `databases` map
// does not cause a fatal error.
func TestLoad_DefaultsFromLegacyBlock(t *testing.T) {
	dir := t.TempDir()
	// Write a config file that uses only the legacy block. The
	// `task:` section is absent, so cfg.Task.Database should
	// default to "default" after Load.
	yaml := []byte(`
server:
  port: 9999
database:
  host: legacy.example
  port: 5432
  user: legacy
  password: legacy
  name: legacydb
  ssl_mode: disable
auth:
  jwt_secret: "x"
  token_ttl: 1h
`)
	if err := os.WriteFile(filepath.Join(dir, "config.default.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	// Save and restore CWD so we don't pollute the test environment.
	oldwd, _ := os.Getwd()
	defer os.Chdir(oldwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def, ok := cfg.Databases["default"]
	if !ok {
		t.Fatal("expected `databases.default` to be synthesized from legacy block")
	}
	if def.Host != "legacy.example" || def.Name != "legacydb" {
		t.Errorf("synthesized default = %+v, want legacy.example/legacydb", def)
	}
	if cfg.Task.Database != "default" {
		t.Errorf("Task.Database = %q, want %q (empty legacy fields → default)", cfg.Task.Database, "default")
	}
	if cfg.System.Database != "default" {
		t.Errorf("System.Database = %q, want %q", cfg.System.Database, "default")
	}
	if cfg.Isolation.DefaultDatabase != "default" {
		t.Errorf("Isolation.DefaultDatabase = %q, want %q", cfg.Isolation.DefaultDatabase, "default")
	}
	if len(cfg.Isolation.AllowedDatabases) != 0 {
		t.Errorf("Isolation.AllowedDatabases = %v, want [] (picker closed by default)", cfg.Isolation.AllowedDatabases)
	}
}

// TestLoad_LegacyDatabaseEnvVars verifies that the legacy
// `DATABASE_*` env vars still override the YAML config when set.
// The pre-refactor code did this through viper's AutomaticEnv
// + a now-removed synthesis block; the new map shape needs an
// explicit alias step in Load() because the env-var name
// (DATABASE_HOST) doesn't match the new config key
// (databases.default.host). Without the alias, a deploy that
// sets `DATABASE_HOST=postgres` in its env would silently fall
// back to the YAML default and fail to connect. This test
// guards the alias by setting DATABASE_HOST and asserting the
// value lands in databases.default.host.
func TestLoad_LegacyDatabaseEnvVars(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`
databases:
  default:
    host: localhost
    port: 5432
    user: okt
    password: okt_dev
    name: okt
    ssl_mode: disable
    max_conns: 20
auth:
  jwt_secret: "x"
  token_ttl: 1h
`)
	if err := os.WriteFile(filepath.Join(dir, "config.default.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	defer os.Chdir(oldwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DATABASE_HOST", "override.example")
	t.Setenv("DATABASE_PORT", "6543")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def, ok := cfg.Databases["default"]
	if !ok {
		t.Fatal("expected databases.default")
	}
	if def.Host != "override.example" {
		t.Errorf("Host = %q, want %q (DATABASE_HOST should override YAML)", def.Host, "override.example")
	}
	if def.Port != 6543 {
		t.Errorf("Port = %d, want %d (DATABASE_PORT should override YAML)", def.Port, 6543)
	}
	// Fields the env didn't set keep their YAML values.
	if def.User != "okt" {
		t.Errorf("User = %q, want %q (YAML preserved when env unset)", def.User, "okt")
	}
	if def.Name != "okt" {
		t.Errorf("Name = %q, want %q", def.Name, "okt")
	}
}

// TestLoad_LegacyTaskEnvVars verifies the legacy `TASK_*` env
// vars still create a `databases.tasks` entry and point
// `task.database` at it. Mirrors the DATABASE_* test above; the
// task side is simpler because the operator either sets the env
// vars or they don't, with no overlap to the default DB.
func TestLoad_LegacyTaskEnvVars(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`
databases:
  default:
    host: localhost
    port: 5432
    user: okt
    password: okt_dev
    name: okt
    ssl_mode: disable
auth:
  jwt_secret: "x"
  token_ttl: 1h
`)
	if err := os.WriteFile(filepath.Join(dir, "config.default.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	defer os.Chdir(oldwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TASK_HOST", "tasks.example")
	t.Setenv("TASK_NAME", "okt_tasks")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tasks, ok := cfg.Databases["tasks"]
	if !ok {
		t.Fatal("expected databases.tasks to be created from TASK_* env vars")
	}
	if tasks.Host != "tasks.example" {
		t.Errorf("tasks.Host = %q, want %q", tasks.Host, "tasks.example")
	}
	if tasks.Name != "okt_tasks" {
		t.Errorf("tasks.Name = %q, want %q", tasks.Name, "okt_tasks")
	}
	if cfg.Task.Database != "tasks" {
		t.Errorf("Task.Database = %q, want %q (TASK_NAME should auto-select tasks)", cfg.Task.Database, "tasks")
	}
}

// TestLoad_LegacyTaskEnvVars_OverridesYAML is the regression
// test for the multi-DB docker-compose path. The production
// YAML has `task.database: default` (single-DB default), but
// docker-compose sets TASK_NAME=okt_tasks to ask for a
// dedicated task DB. The env var must win over the YAML:
// the operator is explicit, and silently using the shared
// pool is the failure mode the docker-compose wiring is
// designed to prevent. The earlier `cfg.Task.Database == ""`
// guard only fired when the YAML left the field empty, so a
// YAML with `task.database: default` would silently keep
// using `default` even after the operator asked for a
// dedicated task DB. This test exercises the fix.
func TestLoad_LegacyTaskEnvVars_OverridesYAML(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`
databases:
  default:
    host: localhost
    port: 5432
    user: okt
    password: okt_dev
    name: okt
    ssl_mode: disable
task:
  database: default
auth:
  jwt_secret: "x"
  token_ttl: 1h
`)
	if err := os.WriteFile(filepath.Join(dir, "config.default.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	defer os.Chdir(oldwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TASK_HOST", "tasks.example")
	t.Setenv("TASK_NAME", "okt_tasks")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Task.Database != "tasks" {
		t.Errorf("Task.Database = %q, want %q (TASK_NAME must override YAML task.database=default)", cfg.Task.Database, "tasks")
	}
	tasks, ok := cfg.Databases["tasks"]
	if !ok {
		t.Fatal("expected databases.tasks to be created from TASK_* env vars")
	}
	if tasks.Host != "tasks.example" {
		t.Errorf("tasks.Host = %q, want %q", tasks.Host, "tasks.example")
	}
	if tasks.Name != "okt_tasks" {
		t.Errorf("tasks.Name = %q, want %q", tasks.Name, "okt_tasks")
	}
}

// TestLoad_ValidatesCrossReferences verifies the validation step
// catches a `task.database` that doesn't exist in the `databases`
// map.
func TestLoad_ValidatesCrossReferences(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`
databases:
  default:
    host: localhost
    port: 5432
    user: okt
    password: okt
    name: okt
    ssl_mode: disable
task:
  database: nope
`)
	if err := os.WriteFile(filepath.Join(dir, "config.default.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	defer os.Chdir(oldwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown task.database; got nil")
	}
}

// TestLoad_RequiresDatabasesDefault ensures Load fails when no
// `databases` map is declared and no legacy `database:` block is
// present, so a misconfigured install fails at boot rather than
// on the first request.
func TestLoad_RequiresDatabasesDefault(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`
server:
  port: 8080
auth:
  jwt_secret: "x"
  token_ttl: 1h
`)
	if err := os.WriteFile(filepath.Join(dir, "config.default.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	defer os.Chdir(oldwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("expected error when no databases declared; got nil")
	}
}

// TestLoad_RejectsUnknownTopLevelKey is the regression test for the
// "tasks stuck as available" incident. The entire `task:` block in
// config.default.yaml was accidentally indented as a child of
// `isolation:`, which made Viper parse it as
// `isolation.task.{database,queues,…}`. Because IsolationConfig has
// no `task` field, the block was silently dropped — River then
// booted with only the catch-all `default` queue, the per-task
// queues (retrieve_source, source_decomposition, …) were never
// declared, and every enqueued job sat in `available` forever.
//
// With UnmarshalExact (ErrorUnused=true) the loader now rejects any
// top-level key the Config struct doesn't know about, so the
// misplacement fails loudly at boot instead of silently degrading.
// This test exercises that guard by feeding a config with a bogus
// top-level key and asserting Load returns an error.
func TestLoad_RejectsUnknownTopLevelKey(t *testing.T) {
	dir := t.TempDir()
	// `bogus_section:` is not a field on Config. The loader must
	// reject it rather than silently dropping it.
	yaml := []byte(`
databases:
  default:
    host: localhost
    port: 5432
    user: okt
    password: okt
    name: okt
    ssl_mode: disable
auth:
  jwt_secret: "x"
  token_ttl: 1h
bogus_section:
  some_field: value
`)
	if err := os.WriteFile(filepath.Join(dir, "config.default.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	defer os.Chdir(oldwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown top-level key `bogus_section`; got nil (UnmarshalExact should reject unused keys)")
	}
}

// TestLoad_RejectsMisplacedTaskUnderIsolation is the direct
// regression test for the incident: a `task:` block indented under
// `isolation:` (which has no `task` field) must fail at boot. The
// pre-fix loader silently dropped the block; the post-fix loader
// (UnmarshalExact + ErrorUnused) rejects it.
func TestLoad_RejectsMisplacedTaskUnderIsolation(t *testing.T) {
	dir := t.TempDir()
	// Note the 2-space indent on `task:` — this is exactly the
	// shape that shipped in config.default.yaml and caused the
	// incident.
	yaml := []byte(`
databases:
  default:
    host: localhost
    port: 5432
    user: okt
    password: okt
    name: okt
    ssl_mode: disable
isolation:
  default_database: default
  allowed_databases: []
  task:
    database: default
    queues:
      retrieve_source: 25
auth:
  jwt_secret: "x"
  token_ttl: 1h
`)
	if err := os.WriteFile(filepath.Join(dir, "config.default.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	defer os.Chdir(oldwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("expected error for `isolation.task.*` (misplaced task block); got nil — UnmarshalExact should reject it")
	}
}
