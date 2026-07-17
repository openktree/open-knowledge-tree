-- 0032_worker_heartbeat.up.sql
--
-- Worker heartbeat table. Each API process (River client) registers
-- itself here on startup and updates last_heartbeat every
-- cfg.Task.HeartbeatInterval (default 1m). The startup + on-demand
-- rescue query uses this table to determine which workers are alive:
-- any running job whose current owner (attempted_by[last]) has a stale
-- or missing heartbeat is considered orphaned and reset to available.
--
-- LOGGED (not UNLOGGED) so it survives a DB restart — after a Postgres
-- crash, river_job persists but river_client (UNLOGGED) is cleared.
-- Keeping heartbeat rows lets the next boot detect which workers are
-- dead and rescue their jobs.
--
-- Lives in okt_system (system-wide, not per-repo). The same migration
-- set runs against every database; the task DB (okt_tasks) is where it
-- matters — that's where river_job lives and where the rescue query
-- joins against this table.

CREATE TABLE IF NOT EXISTS okt_system.okt_worker_heartbeat (
    worker_id      TEXT PRIMARY KEY,          -- River client.ID(), e.g. "host_2026-06-26T19:57:14"
    hostname       TEXT NOT NULL DEFAULT '',   -- os.Hostname() for human-readable identification
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS okt_worker_heartbeat_last_heartbeat_idx
    ON okt_system.okt_worker_heartbeat (last_heartbeat);