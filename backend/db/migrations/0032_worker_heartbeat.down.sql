-- 0032_worker_heartbeat.down.sql
DROP INDEX IF EXISTS okt_system.okt_worker_heartbeat_last_heartbeat_idx;
DROP TABLE IF EXISTS okt_system.okt_worker_heartbeat;