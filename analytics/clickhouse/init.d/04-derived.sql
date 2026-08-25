-- #506 — derived per-row tokens (out-of-band batch; NOT written by the live ingest path).
-- A batch process (analytics/tools/derive_tokens.py, reusing tokenize.py) computes the
-- #508 token for each network_requests row and writes it here; the read API LEFT-JOINs
-- this onto rows by (player_id, ts, entry_fingerprint) so the token is available
-- everywhere (network log, play log, API, offline) from one computation.
--
-- ReplacingMergeTree(scored_at): a re-score (same player_id/ts/entry_fingerprint, newer
-- scored_at) supersedes the prior token. The read-path JOIN must dedupe to the latest
-- (argMax(token, scored_at) or FINAL) since pre-merge duplicates are possible.
--
-- IDs are written verbatim from network_requests (already canonical-lowercase per the
-- forwarder's canonicalV2ID on ingest) — the batch must NOT re-canonicalise.

CREATE TABLE IF NOT EXISTS infinite_streaming.derived_tokens
(
    ts                DateTime64(3, 'UTC')             CODEC(Delta, ZSTD(1)),
    player_id         LowCardinality(String)           CODEC(ZSTD(1)),
    play_id           LowCardinality(String)           CODEC(ZSTD(1)),
    entry_fingerprint UInt64                            CODEC(ZSTD(1)),
    surface           LowCardinality(String)           CODEC(ZSTD(1)),
    token             LowCardinality(String)           CODEC(ZSTD(1)),
    model_version     LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    scored_at         DateTime64(3, 'UTC')             CODEC(Delta, ZSTD(1))
)
ENGINE = ReplacingMergeTree(scored_at)
PARTITION BY toYYYYMMDD(ts)
ORDER BY (player_id, ts, entry_fingerprint)
TTL toDateTime(ts) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;
