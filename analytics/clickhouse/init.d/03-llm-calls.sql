-- LLM call ledger (issue #417, part of epic #412).
--
-- One row per /api/session_chat call (including refused/cancelled
-- ones, so the ledger reflects all spend attempts not just successes).
-- Read by the daily-budget gate (`SELECT sum(cost_usd) FROM llm_calls
-- WHERE ts >= today()`) and by the UI meter that surfaces spend.
--
-- For an existing deployment without this table, apply via:
--   make analytics-migrate SQL-FILE=analytics/clickhouse/init.d/03-llm-calls.sql

CREATE TABLE IF NOT EXISTS infinite_streaming.llm_calls
(
    ts                DateTime64(3, 'UTC')        DEFAULT now64(3),
    session_id        String                      CODEC(ZSTD(1)),
    profile           LowCardinality(String)      CODEC(ZSTD(1)),
    model             LowCardinality(String)      CODEC(ZSTD(1)),
    prompt_version    LowCardinality(String)      CODEC(ZSTD(1)),
    one_shot          UInt8                       DEFAULT 0,

    -- Token + cost accounting. cost_usd is computed in the forwarder
    -- from per-profile pricing × usage; ClickHouse stores the result
    -- so the daily-budget gate is a single sum() query.
    input_tokens      UInt32                      CODEC(ZSTD(1)),
    output_tokens     UInt32                      CODEC(ZSTD(1)),
    cost_usd          Float64                     CODEC(ZSTD(1)),

    -- Operational shape.
    duration_ms       UInt32                      CODEC(ZSTD(1)),
    iterations        UInt16                      DEFAULT 0,
    tool_calls_count  UInt16                      DEFAULT 0,

    -- Outcome. One of:
    --   'ok'              — completed normally.
    --   'budget_exceeded' — refused pre-flight; daily cap was over.
    --   'input_too_large' — refused pre-flight; estimated tokens >
    --                       LLM_MAX_INPUT_TOKENS_PER_CALL.
    --   'error'           — upstream LLM error / network / parse.
    --   'cancelled'       — client disconnected mid-stream; cost_usd
    --                       and tokens reflect partial usage.
    -- Used by /api/llm_budget to count today's spent + by future
    -- diagnostics queries.
    status            LowCardinality(String)      CODEC(ZSTD(1)),

    -- Optional shape detail when status != 'ok'. Free-form short
    -- text, capped at ~200 chars in the writer to keep rows lean.
    error_kind        LowCardinality(String)      DEFAULT '',
    error_detail      String                      CODEC(ZSTD(3))
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY ts
TTL toDateTime(ts) + INTERVAL 90 DAY;

-- For older deployments that may have an early-version table.
ALTER TABLE infinite_streaming.llm_calls
    ADD COLUMN IF NOT EXISTS prompt_version LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS error_kind LowCardinality(String) DEFAULT '',
    ADD COLUMN IF NOT EXISTS error_detail String CODEC(ZSTD(3));

-- A future PR can add `GRANT SELECT ON infinite_streaming.llm_calls
-- TO llm_reader` so the LLM can answer "how much have I cost today?"
-- via its `query` tool. Skipped here because the llm_reader user is
-- created in a sibling init script (02-llm-reader.sql, issue #413)
-- and granting before the user exists would fail. /api/llm_budget
-- uses the default ClickHouse connection, so this isn't blocking.
