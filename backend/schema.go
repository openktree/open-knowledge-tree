package backend

import "embed"

// FS is an embed.FS rooted at db/migrations. It is consumed by
// the dbpool registry at boot to run golang-migrate against every
// database declared in cfg.Databases. Every database gets the
// same migration set so that:
//
//   - The system database (cfg.System.Database) holds the
//     authoritative copy of the okt_system tables (users,
//     sessions, casbin_rule, repositories) and the okt_repository
//     tables for the "shared" tier (rows for repos whose
//     database_name = 'default').
//   - A per-tenant database (e.g. cfg.Databases["iso_8f3a"]) gets
//     the same DDL. After migrations, it carries an empty copy
//     of every okt_system and okt_repository table. The
//     tier-upgrade flow populates the per-tenant DB by copying
//     the affected repository's rows out of the shared DB and
//     writing a mirrored row into the per-tenant DB's
//     okt_system.repositories.
//
// Splitting DDL by migration number (rather than by file role)
// lets us add new tables, indexes, and constraints in a
// versioned sequence the same way every other database-using
// project does, instead of the previous "one file per role,
// concatenated by hand" shape that made it easy to drift.
//
//go:embed db/migrations/*.sql
var MigrationsFS embed.FS
