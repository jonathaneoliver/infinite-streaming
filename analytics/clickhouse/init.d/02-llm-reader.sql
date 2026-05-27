-- llm_reader — restricted ClickHouse user for the AI chat backend's
-- Tier 3 `query(sql)` tool (#497, originally specified in #413).
--
-- The forwarder passes LLM-authored SQL through unmodified; the
-- safety story is "ClickHouse enforces it, the forwarder doesn't
-- have to." All limits below are server-side and readonly_settings=1
-- means the LLM cannot relax any of them via SETTINGS in the query.
--
-- For local dev (Docker Compose, k3d) this file's CREATE USER with
-- IDENTIFIED WITH no_password is fine — ClickHouse is reached only
-- over the analytics docker network, gated behind the dashboard's
-- auth surface. For production deploys, layer an XML overlay at
-- `/etc/clickhouse-server/users.d/llm_reader.xml` that sets a
-- password_sha256_hex on the same user; the forwarder reads its
-- LLM-side credentials from FORWARDER_CLICKHOUSE_LLM_USER /
-- FORWARDER_CLICKHOUSE_LLM_PASSWORD.

CREATE SETTINGS PROFILE IF NOT EXISTS llm_reader_caps SETTINGS
    -- Read-only — no writes, no DDL, no setting changes within the
    -- connection. `readonly = 1` is sufficient: it blocks DML, DDL,
    -- AND further setting changes, so the LLM cannot relax any of
    -- the caps below from its own query.
    readonly = 1,
    -- Wall-clock cap. Most LLM queries return in <500 ms; the few
    -- that explore (group-by across a wide window) get 10s to plan
    -- + scan + return. Anything past that gets cut.
    max_execution_time = 10,
    -- Memory per query. 1 GB covers any reasonable aggregation
    -- over the 30-day retention window; runaway joins get aborted.
    max_memory_usage = 1000000000,
    -- Row count caps. 10K returned is enough for any analysis
    -- the LLM does with `query()` directly — beyond that the LLM
    -- should aggregate server-side first.
    max_result_rows = 10000,
    max_result_bytes = 10000000,
    -- Scan cap — 10M rows is roughly 30 days of session_events for
    -- a single busy player; bounds runaway full-table scans.
    max_rows_to_read = 10000000,
    -- break (not throw) — return what fit, with a `truncated`
    -- indicator the tool surface forwards to the LLM.
    result_overflow_mode = 'break';

CREATE USER IF NOT EXISTS llm_reader IDENTIFIED WITH no_password
    SETTINGS PROFILE llm_reader_caps;

-- SELECT-only on the analytics database. The LLM can read the live
-- tables (session_events, network_requests, control_events,
-- characterization_runs) plus its own spend ledger (llm_calls; see
-- 03-llm-calls.sql).
GRANT SELECT ON infinite_streaming.* TO llm_reader;

-- system.* is forbidden. The LLM cannot introspect query history,
-- user permissions, or running queries.
REVOKE ALL ON system.* FROM llm_reader;
