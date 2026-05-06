-- LLM tool-use reader (issue #413, part of epic #412).
--
-- ClickHouse runs init.d/*.sql only on first start (when /var/lib/clickhouse
-- is empty). To apply this to an existing deployment without wiping data:
--   make analytics-migrate SQL-FILE=analytics/clickhouse/init.d/02-llm-reader.sql
-- All statements below are idempotent (IF NOT EXISTS), so re-running is safe.
--
-- The forwarder's AI-analysis path (`/analytics/api/session_chat`) lets an
-- LLM author SQL via a `query` tool. We pass that SQL through unchecked
-- because all safety lives here, in this user's settings profile, enforced
-- server-side. The LLM literally cannot escape these limits — `readonly = 1`
-- blocks `SET`, and every cap carries the `READONLY` constraint so the
-- profile itself is also unchangeable.
--
-- Identity: `IDENTIFIED WITH no_password` matches the existing `default`
-- user. The threat model is "constrain what *our* code can do over this
-- connection," not "keep network attackers out" — the default user already
-- accepts unauthenticated connections from the same network, with no
-- profile constraints. Adding a password here would be theatre: anyone
-- on the docker bridge / cluster network could just connect as `default`
-- and bypass every limit.

CREATE SETTINGS PROFILE IF NOT EXISTS llm_readonly SETTINGS
    -- readonly=1: only SELECT / SHOW / DESCRIBE / EXISTS; SET is rejected.
    -- readonly=2 would allow SET, defeating the per-setting caps below.
    readonly = 1 READONLY,
    -- 10 s wall-clock per query. A wedged or accidentally-cartesian-join
    -- query gets cut off before it touches anyone else's latency.
    max_execution_time = 10 READONLY,
    -- 1 GB peak memory per query. ClickHouse 24.x evaluates this in the
    -- pipeline, so a streaming aggregate is fine; an in-memory hash join
    -- of two billion-row tables is not. Verified server-enforced.
    max_memory_usage = 1000000000 READONLY,
    -- 10 M rows scanned per query — enforced before aggregation, so
    -- `SELECT n FROM huge_table` aborts with TOO_MANY_ROWS once the
    -- pipeline has read 10 M source rows. This is the real input cap.
    -- Verified server-enforced (Code 158).
    max_rows_to_read = 10000000 READONLY,
    -- max_result_rows / max_result_bytes / result_overflow_mode are
    -- COOPERATIVE in ClickHouse 24.8: the settings are visible and
    -- READONLY, but plain streaming SELECTs are not truncated server-side
    -- at the result cap. We document the intent here so any tooling that
    -- *does* honor them will respect it, and the forwarder's `query` tool
    -- (#415) enforces a hard 10k-row / 10 MB cap when reading the
    -- response. The server-side line of defense is `max_rows_to_read`
    -- above plus `max_execution_time` below.
    max_result_rows = 10000 READONLY,
    max_result_bytes = 10485760 READONLY,
    result_overflow_mode = 'break' READONLY;

CREATE USER IF NOT EXISTS llm_reader
    IDENTIFIED WITH no_password
    SETTINGS PROFILE 'llm_readonly';

-- Read access to the analytics tables. Limited to the
-- infinite_streaming database; the LLM cannot enumerate or read from
-- system.users, system.zookeeper, etc.
GRANT SELECT ON infinite_streaming.* TO llm_reader;

-- A handful of system tables are useful for the LLM to introspect its
-- own environment ("which tables exist", "what's the schema of X",
-- "how big are the tables") without exposing credentials. system.numbers
-- and system.one are universally readable in ClickHouse and don't need
-- an explicit grant.
GRANT SELECT ON system.tables TO llm_reader;
GRANT SELECT ON system.columns TO llm_reader;
GRANT SELECT ON system.parts TO llm_reader;
GRANT SELECT ON system.parts_columns TO llm_reader;
