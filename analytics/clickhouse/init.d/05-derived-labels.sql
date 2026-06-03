-- #506 — derived anomaly LABELS, PER ROW (out-of-band batch; NOT the live ingest path).
-- The VOMM scorer's surprise verdict, anchored on the EXACT row whose token transition was
-- improbable — so it merges onto that row at read time (like derived_tokens) and shows as a
-- chip right where the surprise happened, with as many per session as there are surprising
-- rows. A batch (analytics/tools/derive_labels.py, reusing scorer.py + tokenize.py) trains
-- per-condition VOMMs on older plays and scores recent plays OUT-OF-SAMPLE (time-split),
-- emitting one row per above-threshold transition WITHIN a condition episode.
--
-- Keyed like derived_tokens so the read-path joins line up:
--   * network-row labels → entry_fingerprint = the network_requests row's fingerprint.
--   * event-row labels    → entry_fingerprint = 0, surface = 'event' (join on player_id, ts).
-- One row per (player_id, ts, entry_fingerprint, condition); ReplacingMergeTree(scored_at)
-- supersedes on re-score.
--
-- The read API surfaces these two ways: (1) per-row — merged into the network/event row's
-- labels so it chips in NetworkLog / PlayLog; (2) per-play — rolled up by play_id into
-- label_histogram so sessions.html can filter. label is the `,`/`=`-free filter key
-- (unexpected_<condition>); `score` is the transition's surprise in nats.

CREATE TABLE IF NOT EXISTS infinite_streaming.derived_labels
(
    ts                DateTime64(3, 'UTC')             CODEC(Delta, ZSTD(1)),
    player_id         LowCardinality(String)           CODEC(ZSTD(1)),
    play_id           LowCardinality(String)           CODEC(ZSTD(1)),
    entry_fingerprint UInt64                            CODEC(ZSTD(1)),
    surface           LowCardinality(String)           CODEC(ZSTD(1)),
    condition         LowCardinality(String)           CODEC(ZSTD(1)),
    label             LowCardinality(String)           CODEC(ZSTD(1)),
    severity          LowCardinality(String)           CODEC(ZSTD(1)),
    score             Float32                          CODEC(ZSTD(1)),
    model_version     LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    scored_at         DateTime64(3, 'UTC')             CODEC(Delta, ZSTD(1))
)
ENGINE = ReplacingMergeTree(scored_at)
PARTITION BY toYYYYMMDD(ts)
ORDER BY (player_id, ts, entry_fingerprint, condition)
TTL toDateTime(ts) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;
