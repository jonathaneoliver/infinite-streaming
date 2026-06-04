-- #608 — VOMM model manifest (observability only; NOT the model itself).
--
-- The #608 split decouples TRAINING the VOMM (slow: nightly full 7-day rebuild) from
-- SCORING with it (fast: ~1 min). The actual model artifact — per-condition PPM counts +
-- p99 threshold — is a gzipped-JSON file on a volume shared by the label-trainer and
-- label-scorer sidecars (LABEL_MODEL_PATH, default /models/labels-model.json.gz); it is too
-- large and too internal to belong in a queryable column.
--
-- This table is the small, queryable MANIFEST the trainer writes alongside that artifact:
-- one row per (model_version, condition) per train run, so you can see WHEN the reference
-- model last rebuilt, over how many episodes, and at what threshold — without cracking open
-- the gzip. `trained_at` is also the out-of-sample cutoff the scorer enforces (it only
-- scores plays whose max_ts post-dates the model it loaded), so surfacing it here makes the
-- guarantee auditable.
--
-- ReplacingMergeTree(trained_at) keeps the latest row per (model_version, condition).

CREATE TABLE IF NOT EXISTS infinite_streaming.derived_models
(
    trained_at        DateTime64(3, 'UTC')             CODEC(Delta, ZSTD(1)),
    model_version     LowCardinality(String)           CODEC(ZSTD(1)),
    condition         LowCardinality(String)           CODEC(ZSTD(1)),
    train_window_days UInt16                            CODEC(ZSTD(1)),
    n_train_episodes  UInt32                            CODEC(ZSTD(1)),
    threshold         Float32                          CODEC(ZSTD(1)),
    n_contexts        UInt32                            CODEC(ZSTD(1)),
    vocab             UInt32                            CODEC(ZSTD(1)),
    max_order         UInt8                             CODEC(ZSTD(1)),
    artifact_path     String                 DEFAULT '' CODEC(ZSTD(1)),
    artifact_bytes    UInt64                 DEFAULT 0  CODEC(ZSTD(1))
)
ENGINE = ReplacingMergeTree(trained_at)
PARTITION BY toYYYYMMDD(trained_at)
ORDER BY (model_version, condition)
TTL toDateTime(trained_at) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192;
