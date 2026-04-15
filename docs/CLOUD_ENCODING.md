# Cloud Encoding (AWS EC2 Spot)

InfiniteStream can offload ABR ladder encoding to one-shot EC2 spot instances instead of running it on your local machine. The instance pulls the same Docker image, runs `create_abr_ladder.sh` unchanged, uploads results back to S3, and self-terminates.

Typical 5-minute 1080p clip on `c7i.8xlarge` spot: **~10 min wall-clock, ~$0.15–0.25 per encode.**

Scripts live in `generate_abr/`:
- `aws_setup.sh` — one-time idempotent setup (S3 bucket, IAM role, VPC lookup)
- `cloud_encode.sh` — per-job wrapper (upload → launch → poll → download → cleanup)

## When to use it

- Encoding large clips or long ladders that are slow on your laptop.
- Batch-encoding several clips (the script runs them sequentially on one instance to amortize boot + image-pull cost).
- You don't want to tie up your local machine for an hour.

If a clip takes less than ~2 minutes locally, cloud encoding will be slower end-to-end because of boot + image-pull + upload/download overhead. Stay local for short clips.

## Prerequisites

- An AWS account with permission to create S3 buckets, IAM roles, and EC2 instances.
- `aws` CLI installed and authenticated (`aws sts get-caller-identity` must succeed).
- A GitHub personal access token (PAT) with `read:packages` scope — the remote instance uses it to pull the image from GHCR. Generate at https://github.com/settings/tokens.
- Optional but recommended: `s5cmd` installed locally (~5× faster output download than `aws s3 sync` for many-small-files workloads).

## One-time setup

Run from the repo root:

```bash
./generate_abr/aws_setup.sh
```

You'll be prompted for a globally-unique S3 bucket name (e.g. `<account-id>-encode-staging`). The script:

1. Creates the S3 bucket with a 1-day lifecycle rule on `jobs/` so staging auto-expires.
2. Creates an IAM role (`encode-worker`) + instance profile with S3-only access scoped to that bucket.
3. Resolves the default VPC's subnet and security group for the region.

It prints a block of env vars at the end. Add them to `.env` along with your GHCR PAT:

```
AWS_REGION=us-west-2
S3_BUCKET=<your-bucket>
INSTANCE_PROFILE=encode-worker
SUBNET_ID=subnet-xxxx
SECURITY_GROUP_ID=sg-xxxx
GHCR_PAT=ghp_...
```

The PAT stays on your machine. It is injected into EC2 user-data at launch time only; no persistent AWS secret is created.

## Running an encode

Single clip:

```bash
./generate_abr/cloud_encode.sh --input ./clip.mp4 --codec h264 --max-res 1080p --time 300
```

Batch (one instance, sequential encodes, amortizes boot cost):

```bash
./generate_abr/cloud_encode.sh \
  --input ./a.mp4 --input ./b.mp4 --input ./c.mkv \
  --codec h264 --max-res 1080p
```

Outputs land in `./cloud_output_<JOB_ID>/` unless `--output-dir` is passed.

Any flag that isn't a wrapper flag (below) is forwarded verbatim to `create_abr_ladder.sh`. Do **not** pass `--force-hardware` — the EC2 instance has no VideoToolbox.

### Wrapper flags

| Flag | Effect |
|---|---|
| `--input <path>` | Source file. Required. Repeatable for batch mode. |
| `--output-dir <path>` | Local directory for downloaded outputs (default: `./cloud_output_<JOB_ID>`) |
| `--keep-instance` | Don't terminate the instance on completion (for debugging) |
| `--keep-s3` | Don't delete S3 staging after successful download |
| `--dry-run` | Print the plan and exit; don't touch AWS |
| `--job-id <id>` | Reuse an existing job id (e.g. to re-download outputs from a prior run) |

### Environment overrides

Override via env var at invocation time:

| Var | Default | Notes |
|---|---|---|
| `AWS_REGION` | `us-west-2` | |
| `INSTANCE_TYPE` | `c7i.8xlarge` | |
| `INSTANCE_TYPE_FALLBACKS` | `c7a.8xlarge,c6i.8xlarge` | Tried in order if primary is out of capacity |
| `USE_SPOT` | `true` | Set to `false` for on-demand (~2× cost, near-guaranteed capacity) |
| `DOCKER_IMAGE` | `ghcr.io/jonathaneoliver/infinite-streaming:latest` | Pin to a SHA tag for reproducibility |
| `POLL_INTERVAL` | `20` | Seconds between S3 completion marker checks |
| `POLL_TIMEOUT_PER_CLIP` | `3600` | Per-clip timeout budget |

## What happens on the remote instance

User-data runs on first boot:

1. Install Docker (Amazon Linux 2023 has it in `dnf`).
2. Log in to GHCR with the injected PAT.
3. Pull the image (once, shared across all clips in the batch).
4. Stage every input from S3 to `/work/input/`.
5. For each clip: run `create_abr_ladder.sh` in the container, syncing output to S3 incrementally.
6. Write `_DONE` (or `_FAILED`) marker to S3.
7. `shutdown -h +1` — the instance was launched with shutdown-behavior=terminate, so it self-deletes after ~60s.

Logs stream to S3 at `s3://$S3_BUCKET/jobs/$JOB_ID/logs/user-data.log` in real time. If the job fails, the log is downloaded to `$LOCAL_OUTPUT_DIR/user-data.log` automatically.

## Capacity fallback

Spot capacity for `c7i.8xlarge` can be tight. The wrapper automatically tries:

1. Primary instance type × configured subnet.
2. Primary instance type × every other default-VPC AZ in the region.
3. Each fallback type (`c7a.8xlarge`, `c6i.8xlarge`) × all AZs.

Only `InsufficientInstanceCapacity` triggers the fallback walk. Any other launch error is fatal and printed.

If all candidates fail, you'll see a hint to try `USE_SPOT=false` or to narrow the instance type (e.g. `INSTANCE_TYPE_FALLBACKS='c7i.4xlarge,c7a.4xlarge'`).

## Cost model

Every run prints a per-job estimate at the end, covering EC2 spot (or on-demand), EBS (100 GB gp3 pro-rated), S3 → internet egress, and ~1 day of S3 storage. Estimate is ±15% and excludes S3 request fees and inter-AZ traffic.

**Ongoing ($/month) cost when idle:** effectively $0. The S3 bucket auto-expires `jobs/` after 1 day. IAM roles and instance profiles are free.

The wrapper also prints an "Ongoing-cost check" at the end showing:
- Live EC2 instances tagged `JobId` (target: 0)
- Unattached EBS volumes in the region (target: 0 — orphan volumes cost silently)
- S3 staging bucket footprint

If any of these are non-zero, the script prints ready-to-paste cleanup commands (menu items A–E).

## Troubleshooting

**`error: GHCR_PAT env var is required`** — Add `GHCR_PAT=ghp_...` to your `.env`. The PAT needs `read:packages` scope.

**`aws CLI not authenticated for region`** — Run `aws configure` or set `AWS_PROFILE`. Verify with `aws sts get-caller-identity --region us-west-2`.

**Job times out (`!!! Job did not complete`)** — The wrapper downloads `user-data.log` to `$LOCAL_OUTPUT_DIR` automatically. Read it to see where the remote run failed. Common causes: GHCR login failed (bad PAT), instance can't reach S3 (subnet lacks NAT/IGW), or `create_abr_ladder.sh` itself errored.

**All capacity exhausted** — See the fallback section above. `USE_SPOT=false` is the reliable (but pricier) escape hatch.

**Outputs missing or short** — Check `$LOCAL_OUTPUT_DIR/user-data.log` for errors in the `create_abr_ladder.sh` stage. Re-download with the same `--job-id` as long as S3 staging hasn't expired (default: 1 day). Pass `--keep-s3` on the first run if you want to inspect staging after download.

**"Two --input files share the basename"** — S3 keys would collide. Rename one locally before uploading.

## Full teardown

If you want to remove the infra (all $0 ongoing, but for tidiness):

```bash
aws s3 rm   "s3://$S3_BUCKET/" --recursive
aws s3 rb   "s3://$S3_BUCKET/"
aws iam remove-role-from-instance-profile --instance-profile-name encode-worker --role-name encode-worker
aws iam delete-instance-profile           --instance-profile-name encode-worker
aws iam delete-role-policy --role-name encode-worker --policy-name encode-worker-policy
aws iam delete-role        --role-name encode-worker
```
