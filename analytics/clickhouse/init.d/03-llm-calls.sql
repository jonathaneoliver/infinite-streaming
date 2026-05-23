-- llm_calls — per-call ledger for the AI chat backend (#497).
--
-- One row per request to /api/v2/chat: input/output token counts,
-- USD cost (computed from the profile's published pricing),
-- duration, tool-call count, terminal status. Drives the global
-- daily budget check (sum(cost_usd) WHERE ts >= today() vs cap),
-- and gives operators per-key spend visibility without storing the
-- API key itself.
--
-- Privacy: `key_hash` is sha256 of the user's API key — surfaces
-- "key X spent $4.20 today" without persisting the key. The
-- forwarder never logs the raw key in any path.

CREATE TABLE IF NOT EXISTS infinite_streaming.llm_calls
(
    ts                DateTime64(3, 'UTC') DEFAULT now64(3) CODEC(DoubleDelta, ZSTD(1)),

    -- Conversation correlation. chat_id is per-thread (one Vue
    -- chat panel = one thread); request_id is per-turn.
    chat_id           String                                CODEC(ZSTD(1)),
    request_id        String                                CODEC(ZSTD(1)),

    -- Identity & target.
    -- key_hash = lower(hex(sha256(api_key))) — never the key itself.
    key_hash          FixedString(64)                       CODEC(ZSTD(1)),
    profile           LowCardinality(String)                CODEC(ZSTD(1)),
    base_url          String                                CODEC(ZSTD(1)),
    model             LowCardinality(String)                CODEC(ZSTD(1)),

    -- Conversation scope at request time (for forensics queries).
    one_shot          UInt8                 DEFAULT 0       CODEC(ZSTD(1)),
    scope_kind        LowCardinality(String)                CODEC(ZSTD(1)), -- 'fleet'|'play'|'range'|'characterization'|''
    scope_play_id     LowCardinality(String)                CODEC(ZSTD(1)),
    scope_run_id      LowCardinality(String)                CODEC(ZSTD(1)),

    -- Spend accounting.
    input_tokens      UInt32                DEFAULT 0       CODEC(ZSTD(1)),
    output_tokens     UInt32                DEFAULT 0       CODEC(ZSTD(1)),
    -- cost_usd: -1 means "unknown" (user-customized model whose
    -- pricing isn't in the profile catalog); the budget guard
    -- treats unknown as zero (allowed but not tallied).
    cost_usd          Float64               DEFAULT 0       CODEC(ZSTD(1)),

    -- Outcome.
    duration_ms       UInt32                DEFAULT 0       CODEC(ZSTD(1)),
    tool_calls_count  UInt16                DEFAULT 0       CODEC(ZSTD(1)),
    -- ok | budget_exceeded | input_too_large | error | cancelled
    status            LowCardinality(String)                CODEC(ZSTD(1)),
    error_kind        LowCardinality(String) DEFAULT ''     CODEC(ZSTD(1)),

    -- Prompt version (frontmatter in prompts/chat.md). Correlates
    -- prompt iterations with output quality.
    prompt_version    LowCardinality(String) DEFAULT ''     CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toYYYYMM(ts)
ORDER BY (ts, chat_id, request_id)
TTL toDateTime(ts) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;

-- Grant the llm_reader user SELECT on llm_calls so the LLM can see
-- its own spend ("how much have I cost today?") via the query tool.
-- The user cannot see other users' keys — key_hash is opaque.
GRANT SELECT ON infinite_streaming.llm_calls TO llm_reader;
