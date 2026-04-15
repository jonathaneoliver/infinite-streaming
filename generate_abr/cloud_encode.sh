#!/usr/bin/env bash
# cloud_encode.sh — run create_abr_ladder.sh on a one-shot AWS EC2 spot instance
#
# Flow:
#   1. Upload source clip to s3://$S3_BUCKET/jobs/$JOB_ID/input/
#   2. Launch spot EC2 with user-data that:
#        - logs into GHCR using a PAT stored in AWS Secrets Manager
#        - pulls ghcr.io/jonathaneoliver/infinite-streaming:latest
#        - runs /generate_abr/create_abr_ladder.sh inside the container
#        - uploads outputs back to s3://$S3_BUCKET/jobs/$JOB_ID/output/
#        - writes _DONE (or _FAILED) marker and shuts down (instance auto-terminates)
#   3. Poll S3 for the marker, then sync outputs to $LOCAL_OUTPUT_DIR.
#
# One-time AWS setup (you, manually):
#   - S3 bucket for staging
#   - IAM role "$INSTANCE_PROFILE" with S3 access (scoped to the bucket)
#   - Security group "$SECURITY_GROUP_ID" (egress only is fine)
#   - Subnet "$SUBNET_ID" in a VPC with NAT/IGW for S3 + GHCR access
#   - AMI: Amazon Linux 2023 (has Docker via dnf)
#
# GHCR credentials:
#   The PAT is read at launch time from the $GHCR_PAT env var (put it in .env)
#   and baked into the EC2 user-data. No persistent AWS state is kept.

set -euo pipefail

# =============================================================================
# CONFIG — edit these for your account
# =============================================================================
AWS_REGION="${AWS_REGION:-us-west-2}"
S3_BUCKET="${S3_BUCKET:-CHANGE-ME-encode-staging}"
INSTANCE_TYPE="${INSTANCE_TYPE:-c7i.8xlarge}"
# Comma-separated list of fallback instance types to try if the primary pool
# is empty across all AZs. Different families use different spot pools so this
# rescues most capacity errors. Set to empty string to disable fallback.
INSTANCE_TYPE_FALLBACKS="${INSTANCE_TYPE_FALLBACKS:-c7a.8xlarge,c6i.8xlarge}"
USE_SPOT="${USE_SPOT:-true}"
AMI_ID="${AMI_ID:-}"                         # auto-resolved if empty (AL2023 x86_64)
SUBNET_ID="${SUBNET_ID:-CHANGE-ME}"
SECURITY_GROUP_ID="${SECURITY_GROUP_ID:-CHANGE-ME}"
INSTANCE_PROFILE="${INSTANCE_PROFILE:-encode-worker}"
GHCR_USERNAME="${GHCR_USERNAME:-jonathaneoliver}"
GHCR_PAT="${GHCR_PAT:-}"                     # required — set in .env
DOCKER_IMAGE="${DOCKER_IMAGE:-ghcr.io/jonathaneoliver/infinite-streaming:latest}"
POLL_INTERVAL="${POLL_INTERVAL:-20}"         # seconds between S3 marker checks
POLL_TIMEOUT_PER_CLIP="${POLL_TIMEOUT_PER_CLIP:-3600}"  # seconds of budget per clip
POLL_TIMEOUT="${POLL_TIMEOUT:-}"             # absolute override; otherwise clips*per-clip

# =============================================================================
# USAGE
# =============================================================================
usage() {
    cat <<EOF
Usage: $0 --input <path> [--input <path> ...] [--output-dir <local-dir>] [create_abr_ladder.sh args...]

Wrapper flags (consumed here):
  --input <path>          Local source video file to upload and encode (REQUIRED;
                          may be repeated to encode multiple clips on the same
                          EC2 instance, sequentially)
  --output-dir <path>     Local directory to sync results into (default: ./cloud_output_<JOB_ID>)
                          Multiple clips land in per-clip subdirectories.
  --keep-instance         Don't shut down the instance on completion (for debugging)
  --keep-s3               Don't delete S3 staging after a successful download
                          (default: staging is deleted once local copy is verified)
  --dry-run               Print plan and exit; don't touch AWS
  --job-id <id>           Reuse an existing job id (e.g. to re-download outputs)

All other flags are forwarded verbatim to create_abr_ladder.sh inside the container.
They apply to every clip in the batch (e.g. --codec, --max-res, --time).
Do NOT pass --force-hardware (the EC2 instance has no VideoToolbox).

Examples:
  # single clip
  $0 --input ./clip.mp4 --codec h264 --max-res 1080p --time 300

  # batch of clips on one instance (amortizes boot + image pull)
  $0 --input ./a.mp4 --input ./b.mp4 --input ./c.mkv --codec h264 --max-res 1080p
EOF
    exit 1
}

# =============================================================================
# ARG PARSE
# =============================================================================
INPUTS=()
LOCAL_OUTPUT_DIR=""
KEEP_INSTANCE="false"
KEEP_S3="false"
DRY_RUN="false"
JOB_ID=""
PASSTHROUGH=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --input)          INPUTS+=("$2"); shift 2;;
        --output-dir)     LOCAL_OUTPUT_DIR="$2"; shift 2;;
        --keep-instance)  KEEP_INSTANCE="true"; shift;;
        --keep-s3)        KEEP_S3="true"; shift;;
        --dry-run)        DRY_RUN="true"; shift;;
        --job-id)         JOB_ID="$2"; shift 2;;
        --help|-h)        usage;;
        *)                PASSTHROUGH+=("$1"); shift;;
    esac
done

(( ${#INPUTS[@]} >= 1 )) || { echo "error: at least one --input is required"; usage; }
for f in "${INPUTS[@]}"; do
    [[ -f "$f" ]] || { echo "error: input file not found: $f"; exit 1; }
done

# Reject duplicate basenames (S3 key collision + ambiguous output subdirs)
declare -A SEEN_BASENAMES
for f in "${INPUTS[@]}"; do
    bn="$(basename "$f")"
    if [[ -n "${SEEN_BASENAMES[$bn]:-}" ]]; then
        echo "error: two --input files share the basename '$bn' (${SEEN_BASENAMES[$bn]} and $f)."
        echo "       Rename one locally before uploading, or they'll collide in S3."
        exit 1
    fi
    SEEN_BASENAMES[$bn]="$f"
done

JOB_ID="${JOB_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
LOCAL_OUTPUT_DIR="${LOCAL_OUTPUT_DIR:-./cloud_output_${JOB_ID}}"
S3_PREFIX="s3://${S3_BUCKET}/jobs/${JOB_ID}"

# =============================================================================
# PREFLIGHT
# =============================================================================
command -v aws >/dev/null || { echo "error: aws CLI not installed"; exit 1; }
aws sts get-caller-identity --region "$AWS_REGION" >/dev/null 2>&1 \
    || { echo "error: aws CLI not authenticated for region $AWS_REGION"; exit 1; }
[[ -z "$GHCR_PAT" ]] && {
    echo "error: GHCR_PAT env var is required (add it to .env)"
    echo "       generate one at https://github.com/settings/tokens with read:packages"
    exit 1
}

if [[ -z "$AMI_ID" ]]; then
    AMI_ID="$(aws ssm get-parameter --region "$AWS_REGION" \
        --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 \
        --query 'Parameter.Value' --output text)"
fi

echo "=== cloud_encode plan ==="
echo "  job_id:         $JOB_ID"
echo "  inputs (${#INPUTS[@]}):"
for f in "${INPUTS[@]}"; do echo "    - $f"; done
echo "  s3 prefix:      $S3_PREFIX"
echo "  output dir:     $LOCAL_OUTPUT_DIR"
echo "  region:         $AWS_REGION"
echo "  instance type:  $INSTANCE_TYPE ($([[ "$USE_SPOT" == "true" ]] && echo spot || echo on-demand))"
echo "  ami:            $AMI_ID"
echo "  image:          $DOCKER_IMAGE"
echo "  encode args:    ${PASSTHROUGH[*]:-<none>}"
[[ "$DRY_RUN" == "true" ]] && { echo "(dry-run; exiting)"; exit 0; }

# =============================================================================
# UPLOAD SOURCES
# =============================================================================
for f in "${INPUTS[@]}"; do
    bn="$(basename "$f")"
    local_size=$(stat -f %z "$f" 2>/dev/null || stat -c %s "$f" 2>/dev/null || echo 0)
    # `|| echo` masks the non-zero exit `aws s3 ls` returns when the key is
    # missing — without it, set -o pipefail + set -e exits the script silently.
    remote_size=$( { aws s3 ls "$S3_PREFIX/input/$bn" --region "$AWS_REGION" 2>/dev/null \
        | awk '{print $3}'; } || echo "")
    if [[ -n "$remote_size" && "$remote_size" == "$local_size" ]]; then
        echo ">>> Skipping $bn — already in S3 (same size: $local_size bytes)"
        continue
    fi
    echo ">>> Uploading $bn to $S3_PREFIX/input/"
    aws s3 cp "$f" "$S3_PREFIX/input/$bn" --region "$AWS_REGION"
done

# =============================================================================
# RENDER USER-DATA
# =============================================================================
# Quote the passthrough args for safe embedding.
ENCODE_ARGS=""
for a in "${PASSTHROUGH[@]:-}"; do
    [[ -z "$a" ]] && continue
    ENCODE_ARGS+=" $(printf '%q' "$a")"
done

# Space-separated list of basenames for the user-data loop. No filename here
# will contain whitespace since we reject duplicate basenames; still, we quote.
INPUT_BASENAMES=""
for f in "${INPUTS[@]}"; do
    INPUT_BASENAMES+=" $(printf '%q' "$(basename "$f")")"
done

USER_DATA=$(cat <<EOF
#!/bin/bash
set -euxo pipefail
exec > >(tee /var/log/cloud-encode.log | aws s3 cp - "${S3_PREFIX}/logs/user-data.log" --region "${AWS_REGION}") 2>&1

CURRENT_CLIP="<pre-loop>"

# marker helpers
mark_failed() {
    echo "FAILED at clip '\${CURRENT_CLIP}': \$1" | aws s3 cp - "${S3_PREFIX}/_FAILED" --region "${AWS_REGION}" || true
    shutdown -h +1 "encode failed: \$1"
    exit 1
}
trap 'mark_failed "trap at line \$LINENO"' ERR

# 1. Docker + aws CLI (AL2023 has both)
dnf install -y docker
systemctl enable --now docker

# 2. GHCR login (PAT baked into user-data)
echo "${GHCR_PAT}" | docker login ghcr.io -u "${GHCR_USERNAME}" --password-stdin

# 3. Pull image (once, shared across all clips in this batch)
docker pull "${DOCKER_IMAGE}"

# 4. Stage all inputs
mkdir -p /work/input /work/output /work/tmp
for bn in ${INPUT_BASENAMES}; do
    aws s3 cp "${S3_PREFIX}/input/\${bn}" "/work/input/\${bn}" --region "${AWS_REGION}"
done

# 5. Encode each clip into /work/output/ directly (flat layout).
# create_abr_ladder.sh produces <base>_h264/ and <base>_hevc/ under --output-dir,
# where <base> = value of --output (we set it to "<stem>_p200" so the codec dirs
# end up named "<stem>_p200_h264" / "<stem>_p200_hevc").
# TMPDIR_OUTPUT keeps scratch at /work/tmp (outside /work/output) so it never
# gets synced to S3. TMPDIR also keeps shaka-packager temp files on the same
# filesystem to avoid EXDEV across the container overlay boundary.
for bn in ${INPUT_BASENAMES}; do
    CURRENT_CLIP="\${bn}"
    stem="\${bn%.*}"
    base="\${stem}_p200"

    echo ">>> Encoding \${bn} -> /work/output/\${base}_{h264,hevc}/"
    docker run --rm \
        -v /work:/work \
        -w /work/output \
        -e TMPDIR=/work/tmp \
        -e TMPDIR_OUTPUT=/work/tmp \
        --entrypoint /generate_abr/create_abr_ladder.sh \
        "${DOCKER_IMAGE}" \
        --input "/work/input/\${bn}" \
        --output-dir /work/output \
        --output "\${base}" \
        ${ENCODE_ARGS}

    # Incremental sync of the whole output tree. aws s3 sync is idempotent
    # (ETag comparison), so already-uploaded clips aren't re-sent.
    aws s3 sync /work/output "${S3_PREFIX}/output/" \
        --exclude '*_tmp/*' --exclude '*/abr_ladder_*/*' \
        --region "${AWS_REGION}"
done
CURRENT_CLIP="<post-loop>"

# 6. Final sync (safety net — captures anything sync missed, still excludes scratch)
aws s3 sync /work/output "${S3_PREFIX}/output/" \
    --exclude '*_tmp/*' --exclude '*/abr_ladder_*/*' \
    --region "${AWS_REGION}"

# 7. Success marker
echo "OK" | aws s3 cp - "${S3_PREFIX}/_DONE" --region "${AWS_REGION}"

# 8. Self-terminate (instance has shutdown-terminate behavior set at launch)
shutdown -h +1 "encode complete"
EOF
)

# =============================================================================
# LAUNCH INSTANCE
# =============================================================================
USER_DATA_B64=$(printf '%s' "$USER_DATA" | base64 | tr -d '\n')

MARKET_OPTS=()
[[ "$USE_SPOT" == "true" ]] && MARKET_OPTS=(--instance-market-options 'MarketType=spot')

SHUTDOWN_BEHAVIOR="terminate"
[[ "$KEEP_INSTANCE" == "true" ]] && SHUTDOWN_BEHAVIOR="stop"

# Build subnet candidate list: user's configured SUBNET_ID first, then every
# other default-VPC subnet in the region (so we can fail over on
# InsufficientInstanceCapacity without the user picking another AZ manually).
SUBNET_CANDIDATES=("$SUBNET_ID")
OTHER_SUBNETS=$(aws ec2 describe-subnets --region "$AWS_REGION" \
    --filters "Name=default-for-az,Values=true" \
    --query "Subnets[?SubnetId!='${SUBNET_ID}'].SubnetId" \
    --output text 2>/dev/null || true)
for s in $OTHER_SUBNETS; do
    SUBNET_CANDIDATES+=("$s")
done

# Build instance type candidate list: primary INSTANCE_TYPE first, then the
# fallbacks. Different families have independent spot pools.
INSTANCE_CANDIDATES=("$INSTANCE_TYPE")
if [[ -n "$INSTANCE_TYPE_FALLBACKS" ]]; then
    IFS=',' read -r -a _fallbacks <<< "$INSTANCE_TYPE_FALLBACKS"
    for t in "${_fallbacks[@]}"; do
        # Skip empty entries and duplicates of the primary.
        [[ -z "$t" || "$t" == "$INSTANCE_TYPE" ]] && continue
        INSTANCE_CANDIDATES+=("$t")
    done
fi

INSTANCE_ID=""
LAUNCHED_TYPE=""
LAUNCH_ERR=""
for try_type in "${INSTANCE_CANDIDATES[@]}"; do
    for subnet in "${SUBNET_CANDIDATES[@]}"; do
        echo ">>> Launching $try_type in subnet $subnet"
        if INSTANCE_ID=$(aws ec2 run-instances --region "$AWS_REGION" \
            --image-id "$AMI_ID" \
            --instance-type "$try_type" \
            --subnet-id "$subnet" \
            --security-group-ids "$SECURITY_GROUP_ID" \
            --iam-instance-profile "Name=$INSTANCE_PROFILE" \
            --instance-initiated-shutdown-behavior "$SHUTDOWN_BEHAVIOR" \
            --user-data "$USER_DATA_B64" \
            --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=100,VolumeType=gp3}' \
            --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=encode-$JOB_ID},{Key=JobId,Value=$JOB_ID}]" \
            "${MARKET_OPTS[@]}" \
            --query 'Instances[0].InstanceId' --output text 2> /tmp/cloud_encode_launch_err.$$); then
            SUBNET_ID="$subnet"
            INSTANCE_TYPE="$try_type"
            LAUNCHED_TYPE="$try_type"
            break 2
        fi
        LAUNCH_ERR=$(cat /tmp/cloud_encode_launch_err.$$)
        rm -f /tmp/cloud_encode_launch_err.$$
        if grep -q 'InsufficientInstanceCapacity' <<< "$LAUNCH_ERR"; then
            echo "    no capacity for $try_type in $subnet — trying next AZ"
            INSTANCE_ID=""
            continue
        fi
        # Any other error is fatal — print and abort
        echo "$LAUNCH_ERR" >&2
        exit 1
    done
    if [[ -z "$INSTANCE_ID" ]]; then
        echo "    all AZs exhausted for $try_type — trying next instance type"
    fi
done

if [[ -z "$INSTANCE_ID" ]]; then
    echo "!!! All candidates exhausted: ${INSTANCE_CANDIDATES[*]} × ${#SUBNET_CANDIDATES[@]} AZs."
    echo "    Spot capacity is tight region-wide right now."
    echo "    Try: USE_SPOT=false ... (on-demand, ~2x cost but near-guaranteed)"
    echo "    Or:  INSTANCE_TYPE_FALLBACKS='c7i.4xlarge,c7a.4xlarge' (smaller pools, often available)"
    exit 1
fi
echo "    instance: $INSTANCE_ID ($LAUNCHED_TYPE in $SUBNET_ID)"

cleanup() {
    if [[ "$KEEP_INSTANCE" != "true" ]]; then
        echo ">>> Terminating $INSTANCE_ID (safety net)"
        aws ec2 terminate-instances --region "$AWS_REGION" \
            --instance-ids "$INSTANCE_ID" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# =============================================================================
# POLL FOR COMPLETION
# =============================================================================
if [[ -z "$POLL_TIMEOUT" ]]; then
    POLL_TIMEOUT=$(( ${#INPUTS[@]} * POLL_TIMEOUT_PER_CLIP ))
fi
echo ">>> Waiting for completion marker at $S3_PREFIX/_DONE (timeout ${POLL_TIMEOUT}s for ${#INPUTS[@]} clip(s))"
ELAPSED=0
STATUS=""
while (( ELAPSED < POLL_TIMEOUT )); do
    if aws s3 ls "$S3_PREFIX/_DONE" --region "$AWS_REGION" >/dev/null 2>&1; then
        STATUS="done"; break
    fi
    if aws s3 ls "$S3_PREFIX/_FAILED" --region "$AWS_REGION" >/dev/null 2>&1; then
        STATUS="failed"; break
    fi
    sleep "$POLL_INTERVAL"
    ELAPSED=$((ELAPSED + POLL_INTERVAL))
    printf '    [%4ds] still encoding...\n' "$ELAPSED"
done

if [[ "$STATUS" != "done" ]]; then
    echo "!!! Job did not complete (status=$STATUS). Fetching user-data log."
    mkdir -p "$LOCAL_OUTPUT_DIR"
    aws s3 cp "$S3_PREFIX/logs/user-data.log" "$LOCAL_OUTPUT_DIR/user-data.log" \
        --region "$AWS_REGION" || true
    exit 2
fi

# =============================================================================
# DOWNLOAD RESULTS
# =============================================================================
# Prefer s5cmd if present — ~5–8× faster than aws s3 sync on many-small-files
# workloads (hundreds of .m4s segments per clip × rungs × codecs).
# Falls back to aws CLI if s5cmd isn't installed.
mkdir -p "$LOCAL_OUTPUT_DIR"
if command -v s5cmd >/dev/null 2>&1; then
    echo ">>> Syncing outputs to $LOCAL_OUTPUT_DIR (via s5cmd)"
    AWS_REGION="$AWS_REGION" s5cmd --numworkers 256 sync \
        "$S3_PREFIX/output/*" "$LOCAL_OUTPUT_DIR/"
else
    echo ">>> Syncing outputs to $LOCAL_OUTPUT_DIR (via aws cli; install s5cmd for ~5× speedup)"
    aws s3 sync "$S3_PREFIX/output/" "$LOCAL_OUTPUT_DIR/" --region "$AWS_REGION"
fi
aws s3 cp "$S3_PREFIX/logs/user-data.log" "$LOCAL_OUTPUT_DIR/user-data.log" \
    --region "$AWS_REGION" || true

# Verify local copy is non-empty before we consider cleanup safe
LOCAL_FILE_COUNT=$(find "$LOCAL_OUTPUT_DIR" -type f ! -name 'user-data.log' 2>/dev/null | wc -l | tr -d ' ')
if [[ "$KEEP_S3" == "true" ]]; then
    echo ">>> Leaving S3 staging at $S3_PREFIX (--keep-s3 set)"
elif [[ "$LOCAL_FILE_COUNT" -lt 1 ]]; then
    echo "!!! Local output is empty; leaving S3 staging at $S3_PREFIX for inspection"
else
    echo ">>> Cleaning up S3 staging at $S3_PREFIX ($LOCAL_FILE_COUNT local files verified)"
    aws s3 rm "$S3_PREFIX/" --recursive --region "$AWS_REGION" >/dev/null
fi

echo ">>> Done. Outputs in $LOCAL_OUTPUT_DIR"

# =============================================================================
# THIS-JOB COST ESTIMATE
# =============================================================================
# Rough ±15%. Doesn't include pennies of S3 PUT/GET requests.
# Wrapped in `set +e` so any parse error can't suppress the ongoing-cost check
# or the cleanup-command menu below.
set +e
echo ""
echo "=== Estimated cost of this encode ==="

# On-demand hourly rates for c-family compute-optimized, us-west-2 / us-east-1.
# Other instance types fall through to "unknown" and skip the EC2 estimate.
case "$INSTANCE_TYPE" in
    c7i.large)    OD_HOURLY="0.0892";;
    c7i.xlarge)   OD_HOURLY="0.1785";;
    c7i.2xlarge)  OD_HOURLY="0.3570";;
    c7i.4xlarge)  OD_HOURLY="0.7140";;
    c7i.8xlarge)  OD_HOURLY="1.4280";;
    c7i.12xlarge) OD_HOURLY="2.1420";;
    c7i.16xlarge) OD_HOURLY="2.8560";;
    c7i.24xlarge) OD_HOURLY="4.2840";;
    c6i.4xlarge)  OD_HOURLY="0.6800";;
    c6i.8xlarge)  OD_HOURLY="1.3600";;
    *)            OD_HOURLY="";;
esac

HOURLY="$OD_HOURLY"
PRICING_MODE="on-demand"
if [[ "$USE_SPOT" == "true" && -n "$OD_HOURLY" ]]; then
    # --max-items emits a NextToken ("None") on a second line; keep only the price.
    SPOT=$(aws ec2 describe-spot-price-history --region "$AWS_REGION" \
        --instance-types "$INSTANCE_TYPE" \
        --product-descriptions "Linux/UNIX" \
        --query 'SpotPriceHistory[0].SpotPrice' --output text 2>/dev/null \
        | head -n1 | tr -d '[:space:]')
    # Reject empty, "None", or anything that isn't a decimal number.
    if [[ "$SPOT" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
        HOURLY="$SPOT"
        PRICING_MODE="spot"
    fi
fi

# Add 60s for boot + shutdown overhead not captured in ELAPSED
BILLED_SEC=$((ELAPSED + 60))

if [[ -n "$HOURLY" ]]; then
    EC2_COST=$(awk -v h="$HOURLY" -v s="$BILLED_SEC" 'BEGIN{printf "%.4f", h*s/3600}')
    printf "  EC2 (%s, %s): %ds @ \$%s/hr = \$%s\n" \
        "$INSTANCE_TYPE" "$PRICING_MODE" "$BILLED_SEC" "$HOURLY" "$EC2_COST"
else
    EC2_COST="0"
    echo "  EC2 ($INSTANCE_TYPE): rate unknown, not estimated"
fi

# EBS: 100 GB gp3 at $0.08/GB-month pro-rated over billed seconds
EBS_COST=$(awk -v s="$BILLED_SEC" 'BEGIN{printf "%.4f", 100*0.08*s/(30*24*3600)}')
printf "  EBS (100 GB gp3, pro-rated):        \$%s\n" "$EBS_COST"

# Normalize a possibly-blank/garbage value to a non-negative number (default 0).
_num() { [[ "$1" =~ ^[0-9]+(\.[0-9]+)?$ ]] && echo "$1" || echo "0"; }

# Output bytes — used for egress + 1-day S3 storage
OUTPUT_BYTES=$(du -sk "$LOCAL_OUTPUT_DIR" 2>/dev/null | awk '{print $1*1024}')
OUTPUT_BYTES=$(_num "${OUTPUT_BYTES:-0}")
OUTPUT_GB=$(awk -v b="$OUTPUT_BYTES" 'BEGIN{printf "%.3f", b/1024/1024/1024}')

# Egress: $0.09/GB for first 10 TB (S3 → internet)
EGRESS_COST=$(awk -v g="$OUTPUT_GB" 'BEGIN{printf "%.4f", g*0.09}')
printf "  S3 → internet egress (%s GB):       \$%s\n" "$OUTPUT_GB" "$EGRESS_COST"

# S3 storage: total input across ALL clips + outputs, held ~1 day at $0.023/GB-month
INPUT_BYTES_TOTAL=0
for f in "${INPUTS[@]}"; do
    sz=$(stat -f %z "$f" 2>/dev/null || stat -c %s "$f" 2>/dev/null || echo 0)
    sz=$(_num "$sz")
    INPUT_BYTES_TOTAL=$((INPUT_BYTES_TOTAL + sz))
done
INPUT_GB=$(awk -v b="$INPUT_BYTES_TOTAL" 'BEGIN{printf "%.3f", b/1024/1024/1024}')
S3_COST=$(awk -v g1="$INPUT_GB" -v g2="$OUTPUT_GB" \
    'BEGIN{printf "%.4f", (g1+g2)*0.023/30}')
printf "  S3 storage (~1 day, %s+%s GB):      \$%s\n" "$INPUT_GB" "$OUTPUT_GB" "$S3_COST"

# All four components are guaranteed numeric at this point (awk printf'd them).
TOTAL=$(awk -v a="$EC2_COST" -v b="$EBS_COST" -v c="$EGRESS_COST" -v d="$S3_COST" \
    'BEGIN{printf "%.4f", a+b+c+d}')
printf "  ────────────────────────────────────────\n"
printf "  Estimated total:                     \$%s\n" "$TOTAL"
echo "  (±15%; excludes S3 request fees and inter-AZ traffic)"
set -e

# =============================================================================
# POST-ENCODE COST SUMMARY
# =============================================================================
echo ""
echo "=== Ongoing-cost check ==="

# Wait (up to 60s) for this job's instance to leave the billable states so the
# summary reflects reality. The user-data ends with `shutdown -h +1`, which
# keeps the box 'running' for ~60s after _DONE is written.
printf "  Waiting for %s to terminate" "$INSTANCE_ID"
WAIT_ELAPSED=0
while (( WAIT_ELAPSED < 60 )); do
    STATE=$(aws ec2 describe-instances --region "$AWS_REGION" \
        --instance-ids "$INSTANCE_ID" \
        --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo "unknown")
    if [[ "$STATE" == "shutting-down" || "$STATE" == "terminated" ]]; then
        echo " ($STATE)"
        break
    fi
    printf "."
    sleep 5
    WAIT_ELAPSED=$((WAIT_ELAPSED + 5))
done
[[ "$STATE" != "shutting-down" && "$STATE" != "terminated" ]] && \
    echo " (still $STATE after 60s — will still appear in the count below)"

LIVE_INSTANCES=$(aws ec2 describe-instances --region "$AWS_REGION" \
    --filters "Name=tag:JobId,Values=*" \
        "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'length(Reservations[].Instances[])' --output text 2>/dev/null || echo "?")
echo "  EC2 encode instances still alive:   $LIVE_INSTANCES  (target: 0)"

# Unattached EBS volumes (orphaned = silent cost)
ORPHAN_VOLUMES=$(aws ec2 describe-volumes --region "$AWS_REGION" \
    --filters "Name=status,Values=available" \
    --query 'length(Volumes[])' --output text 2>/dev/null || echo "?")
echo "  Unattached EBS volumes (any job):   $ORPHAN_VOLUMES  (target: 0)"

# S3 staging footprint
S3_STATS=$(aws s3 ls "s3://$S3_BUCKET/" --recursive --summarize 2>/dev/null \
    | awk '/Total Objects/{o=$3} /Total Size/{s=$3} END{printf "%s objects, %.2f GB", o, s/1024/1024/1024}')
echo "  S3 staging bucket ($S3_BUCKET):     $S3_STATS  (auto-deletes after 1 day)"

cat <<EOF

=== Cleanup commands (copy/paste as needed) ===

# A. Delete this job's S3 staging now (you already have local copies):
aws s3 rm "$S3_PREFIX/" --recursive

# B. Wipe ALL staged jobs in the bucket:
aws s3 rm "s3://$S3_BUCKET/" --recursive

# C. Kill any lingering encode instances (shouldn't normally be needed):
aws ec2 describe-instances --region "$AWS_REGION" \\
    --filters "Name=tag:JobId,Values=*" "Name=instance-state-name,Values=pending,running,stopping,stopped" \\
    --query 'Reservations[].Instances[].InstanceId' --output text \\
  | xargs -r aws ec2 terminate-instances --region "$AWS_REGION" --instance-ids

# D. Delete any unattached EBS volumes in this region:
aws ec2 describe-volumes --region "$AWS_REGION" --filters "Name=status,Values=available" \\
    --query 'Volumes[].VolumeId' --output text \\
  | xargs -n1 -r aws ec2 delete-volume --region "$AWS_REGION" --volume-id

# E. Full teardown (removes bucket + IAM — already \$0/mo ongoing):
aws s3 rm "s3://$S3_BUCKET/" --recursive
aws s3 rb "s3://$S3_BUCKET/"
aws iam remove-role-from-instance-profile --instance-profile-name encode-worker --role-name encode-worker
aws iam delete-instance-profile --instance-profile-name encode-worker
aws iam delete-role-policy --role-name encode-worker --policy-name encode-worker-policy
aws iam delete-role --role-name encode-worker
EOF
