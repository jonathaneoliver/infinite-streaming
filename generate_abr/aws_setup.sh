#!/usr/bin/env bash
# aws_setup.sh — one-time AWS setup for cloud_encode.sh
#
# Creates (idempotently):
#   1. S3 bucket for staging (with 1-day lifecycle on jobs/)
#   2. IAM role + instance profile with S3 access
#   3. Resolves a default VPC subnet + security group for the region
#
# Prints the resulting env vars at the end — paste them into your shell or .env
# so cloud_encode.sh picks them up. You also need to add GHCR_PAT=<token> to .env
# so cloud_encode.sh can log in to GHCR on the remote instance.
#
# Re-running is safe: existing resources are detected and reused.

set -euo pipefail

AWS_REGION="${AWS_REGION:-us-west-2}"
S3_BUCKET="${S3_BUCKET:-}"                           # required, will prompt if empty
ROLE_NAME="${ROLE_NAME:-encode-worker}"
INSTANCE_PROFILE="${INSTANCE_PROFILE:-encode-worker}"

command -v aws >/dev/null || { echo "error: aws CLI not installed"; exit 1; }

ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text 2>/dev/null) \
    || { echo "error: aws CLI not authenticated"; exit 1; }
echo "=== Setting up cloud-encode infra in account $ACCOUNT_ID, region $AWS_REGION ==="

# =============================================================================
# 1. S3 BUCKET
# =============================================================================
if [[ -z "$S3_BUCKET" ]]; then
    read -r -p "S3 bucket name (globally unique, e.g. ${ACCOUNT_ID}-encode-staging): " S3_BUCKET
fi

if aws s3api head-bucket --bucket "$S3_BUCKET" --region "$AWS_REGION" 2>/dev/null; then
    echo "[1/4] S3 bucket $S3_BUCKET already exists"
else
    echo "[1/4] Creating S3 bucket $S3_BUCKET"
    if [[ "$AWS_REGION" == "us-east-1" ]]; then
        aws s3api create-bucket --bucket "$S3_BUCKET" --region "$AWS_REGION"
    else
        aws s3api create-bucket --bucket "$S3_BUCKET" --region "$AWS_REGION" \
            --create-bucket-configuration "LocationConstraint=$AWS_REGION"
    fi
    aws s3api put-bucket-versioning --bucket "$S3_BUCKET" \
        --versioning-configuration Status=Suspended
fi
# always (re)apply lifecycle so existing buckets get the latest policy
aws s3api put-bucket-lifecycle-configuration --bucket "$S3_BUCKET" \
    --lifecycle-configuration '{
        "Rules":[{
            "ID":"expire-encode-jobs",
            "Status":"Enabled",
            "Filter":{"Prefix":"jobs/"},
            "Expiration":{"Days":1}
        }]
    }'

# =============================================================================
# 2. IAM ROLE + INSTANCE PROFILE
# =============================================================================
TRUST_POLICY='{
  "Version":"2012-10-17",
  "Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]
}'

if aws iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
    echo "[2/4] IAM role $ROLE_NAME already exists"
else
    echo "[2/4] Creating IAM role $ROLE_NAME"
    aws iam create-role --role-name "$ROLE_NAME" \
        --assume-role-policy-document "$TRUST_POLICY" >/dev/null
fi

INLINE_POLICY=$(cat <<EOF
{
  "Version":"2012-10-17",
  "Statement":[
    {
      "Effect":"Allow",
      "Action":["s3:GetObject","s3:PutObject","s3:ListBucket","s3:DeleteObject"],
      "Resource":["arn:aws:s3:::${S3_BUCKET}","arn:aws:s3:::${S3_BUCKET}/*"]
    }
  ]
}
EOF
)
aws iam put-role-policy --role-name "$ROLE_NAME" \
    --policy-name "encode-worker-policy" \
    --policy-document "$INLINE_POLICY"
echo "      attached inline policy"

if aws iam get-instance-profile --instance-profile-name "$INSTANCE_PROFILE" >/dev/null 2>&1; then
    echo "      instance profile $INSTANCE_PROFILE already exists"
else
    aws iam create-instance-profile --instance-profile-name "$INSTANCE_PROFILE" >/dev/null
    aws iam add-role-to-instance-profile \
        --instance-profile-name "$INSTANCE_PROFILE" --role-name "$ROLE_NAME"
    echo "      created instance profile and attached role"
    echo "      (waiting 10s for IAM propagation)"
    sleep 10
fi

# =============================================================================
# 3. DEFAULT VPC SUBNET + SECURITY GROUP
# =============================================================================
echo "[3/3] Resolving default VPC subnet + security group"
DEFAULT_VPC=$(aws ec2 describe-vpcs --region "$AWS_REGION" \
    --filters Name=is-default,Values=true \
    --query 'Vpcs[0].VpcId' --output text)

if [[ "$DEFAULT_VPC" == "None" || -z "$DEFAULT_VPC" ]]; then
    echo "      no default VPC in this region — create one or set SUBNET_ID/SECURITY_GROUP_ID manually"
    SUBNET_ID=""
    SECURITY_GROUP_ID=""
else
    SUBNET_ID=$(aws ec2 describe-subnets --region "$AWS_REGION" \
        --filters "Name=vpc-id,Values=$DEFAULT_VPC" "Name=default-for-az,Values=true" \
        --query 'Subnets[0].SubnetId' --output text)
    SECURITY_GROUP_ID=$(aws ec2 describe-security-groups --region "$AWS_REGION" \
        --filters "Name=vpc-id,Values=$DEFAULT_VPC" "Name=group-name,Values=default" \
        --query 'SecurityGroups[0].GroupId' --output text)
    echo "      vpc=$DEFAULT_VPC subnet=$SUBNET_ID sg=$SECURITY_GROUP_ID"
fi

# =============================================================================
# SUMMARY
# =============================================================================
cat <<EOF

=== Setup complete ===

Paste these into your shell (or into .env) before running cloud_encode.sh:

    AWS_REGION=$AWS_REGION
    S3_BUCKET=$S3_BUCKET
    INSTANCE_PROFILE=$INSTANCE_PROFILE
    SUBNET_ID=$SUBNET_ID
    SECURITY_GROUP_ID=$SECURITY_GROUP_ID

Also add your GHCR personal access token (scope: read:packages) to .env:

    GHCR_PAT=ghp_...

(Generate one at https://github.com/settings/tokens — this token stays on your
machine; it is only injected into EC2 user-data at encode time.)

Then kick off an encode:

    ./generate_abr/cloud_encode.sh --input ./clip.mp4 --codec h264 --max-res 1080p
EOF
