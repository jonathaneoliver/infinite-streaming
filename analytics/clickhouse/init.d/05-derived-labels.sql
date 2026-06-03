-- #506 — derived anomaly LABELS (out-of-band batch; NOT written by the live ingest path).
-- The sibling of derived_tokens: where derived_tokens carries the per-row #508 token,
-- derived_labels carries the VOMM scorer's per-(play,condition) SURPRISE verdict. A batch
-- (analytics/tools/derive_labels.py, reusing scorer.py + tokenize.py) trains per-condition
-- VOMMs on older plays and scores recent plays OUT-OF-SAMPLE (time-split), writing one row
-- per (play, condition) whose worst episode is notably more surprising than the typical
-- episode for that condition. The read API unions these into labels[] so sessions.html can
-- filter on them and session-viewer can mark them.
--
-- Granularity (v1): one row per (player_id, play_id, condition) — a play-level rollup keyed
-- so ReplacingMergeTree(scored_at) supersedes on re-score. `ts` is the play's first ts
-- (STABLE across re-scores so dedup + partition stay put); `peak_at` is the worst episode's
-- anchor ts (for a precise session-viewer marker later).
--
-- label is the filter key — a clean `,`/`=`-free condition tag (vomm_<condition>_surprise);
-- the model's peak transition (full of `,`/`(`/spaces the label vocab forbids) rides in the
-- `peak` String column as detail, NOT as the filter token.

CREATE TABLE IF NOT EXISTS infinite_streaming.derived_labels
(
    ts             DateTime64(3, 'UTC')             CODEC(Delta, ZSTD(1)),
    player_id      LowCardinality(String)           CODEC(ZSTD(1)),
    play_id        LowCardinality(String)           CODEC(ZSTD(1)),
    condition      LowCardinality(String)           CODEC(ZSTD(1)),
    label          LowCardinality(String)           CODEC(ZSTD(1)),
    severity       LowCardinality(String)           CODEC(ZSTD(1)),
    score          Float32                          CODEC(ZSTD(1)),
    peak           String                           CODEC(ZSTD(1)),
    peak_at        DateTime64(3, 'UTC')             CODEC(Delta, ZSTD(1)),
    model_version  LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    scored_at      DateTime64(3, 'UTC')             CODEC(Delta, ZSTD(1))
)
ENGINE = ReplacingMergeTree(scored_at)
PARTITION BY toYYYYMMDD(ts)
ORDER BY (player_id, play_id, condition)
TTL toDateTime(ts) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;
