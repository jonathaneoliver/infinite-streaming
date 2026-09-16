#!/bin/sh
# Renew the test-dev TLS certificate and deploy it to the box.
#
# WHY THIS EXISTS
#
# test-dev serves https://dev.jeoliver.com:21000 with a Let's Encrypt cert that
# was issued by hand and copied to the box. Nothing renewed it, so on
# 2026-08-17 it expired mid-session: `curl -sk` kept working (it skips
# verification), the already-connected iPhone kept playing on its established
# TLS session, and the first thing to actually fail was a Node `fetch` — by
# which point a demo take had already been lost. Anything that opens a NEW
# connection breaks, including the iOS app, which validates the chain.
#
# HOW THE CERT IS WIRED
#
#   ~/certs/lego/                          lego state (account + cert), on the Mac
#     certificates/dev.jeoliver.com.{crt,key}
#          |  copied
#   box:~/test-dev/certs/localhost{,-key}.pem
#          |  symlinked by the image
#   container:/etc/nginx/certs/localhost.pem
#
# dev.jeoliver.com resolves to a private LAN address, so Let's Encrypt cannot
# reach it for an HTTP-01 challenge. Issuance is DNS-01 through Cloudflare,
# which hosts the jeoliver.com zone.
#
# THE TOKEN
#
# Read from the login keychain, never from a file in the repo or from a shell
# rc. Store it once with:
#
#   security add-generic-password -a "$USER" -s smashing-cloudflare-dns \
#       -w '<cloudflare-api-token>' -U
#
# The token needs Zone:DNS:Edit on jeoliver.com and nothing else.
#
# USAGE
#
#   tools/renew-test-dev-cert.sh          renew if due (<30 days left), then deploy
#   FORCE=1 tools/renew-test-dev-cert.sh  renew regardless of days remaining
#   CHECK=1 tools/renew-test-dev-cert.sh  report expiry and exit, change nothing
#
# Safe to run on a timer: lego does nothing until the cert is inside the
# renewal window, and the deploy step is skipped when the cert did not change.
set -eu

DOMAIN="${CERT_DOMAIN:-dev.jeoliver.com}"
EMAIL="${CERT_EMAIL:-jonathaneoliver@gmail.com}"
LEGO_PATH="${LEGO_PATH:-$HOME/certs/lego}"
KEYCHAIN_SERVICE="${KEYCHAIN_SERVICE:-smashing-cloudflare-dns}"
CONTAINER="${TEST_CONTAINER:-test-dev-server}"
REMOTE_DIR="${REMOTE_CERT_DIR:-test-dev/certs}"
PORT="${TEST_HTTPS_PORT:-21000}"
# go-proxy's control port and the first per-session port. Both terminate TLS
# with the same certificate, independently of nginx, and are what the PLAYER
# talks to — so they are the ports that decide whether playback works.
PROXY_PORTS="${TEST_PROXY_PORTS:-21081 21181}"
RENEW_DAYS="${RENEW_DAYS:-30}"

CRT="$LEGO_PATH/certificates/$DOMAIN.crt"
KEY="$LEGO_PATH/certificates/$DOMAIN.key"

# TEST_SSH lives in .env (gitignored). Resolve it relative to this script so the
# timer does not depend on the working directory.
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ENV_FILE="${ENV_FILE:-$HERE/../.env}"
if [ -z "${TEST_SSH:-}" ] && [ -f "$ENV_FILE" ]; then
    TEST_SSH=$(grep -E '^TEST_SSH=' "$ENV_FILE" | head -1 | cut -d= -f2-)
fi
: "${TEST_SSH:?TEST_SSH not set and not found in $ENV_FILE}"

say() { printf '%s\n' "$*"; }

# Expiry as seen BY A CLIENT, not as recorded on disk — the file and what nginx
# is actually serving can disagree, and only the served one matters.
served_expiry() {
    port="${1:-$PORT}"
    echo | openssl s_client -connect "$DOMAIN:$port" -servername "$DOMAIN" 2>/dev/null \
        | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2
}

deploy() {
    say ""
    # BRACES ARE LOAD-BEARING. "$TEST_SSH…" makes sh read the ellipsis's UTF-8
    # bytes as part of the variable name, so it expands to nothing and `set -u`
    # kills the script — which is exactly how a successful renewal ended up
    # undeployed the first time this ran.
    say "deploying to ${TEST_SSH}…"
    # Both the lego .crt and the deployed pem carry leaf + intermediate, so this
    # is a straight copy rather than a chain rebuild.
    scp -q "$CRT" "$TEST_SSH:$REMOTE_DIR/localhost.pem"
    scp -q "$KEY" "$TEST_SSH:$REMOTE_DIR/localhost-key.pem"
    ssh "$TEST_SSH" "chmod 600 $REMOTE_DIR/localhost-key.pem"

    # RESTART, not `nginx -s reload`.
    #
    # nginx is not the only TLS terminator in this container. go-proxy serves
    # the control port (21081) and every per-session port (21181-21881) with
    # the same certificate, loaded into memory at startup — a signal to nginx
    # does nothing for it.
    #
    # Reloading only nginx produces the worst possible half-fix, and it did:
    # the dashboard and the catalogue came back (both :21000, nginx) while
    # PLAYBACK stayed broken, because the player's media and session traffic go
    # through go-proxy's ports, which were still presenting the expired chain
    # for iOS to reject. Everything looked healthy from a browser.
    #
    # A restart drops connected sessions. That is the correct trade for a
    # certificate change: they cannot survive it anyway.
    say "restarting $CONTAINER (go-proxy holds the cert in memory too)…"
    ssh "$TEST_SSH" "docker restart $CONTAINER" >/dev/null
    sleep 15

    # nginx reloads gracefully: old workers keep answering until their existing
    # connections drain, so an immediate check can still be handed the OLD
    # certificate and report the deploy as failed when it worked. Poll.
    # Verify EVERY terminator, not just nginx. Checking only :21000 is what let
    # a half-deployed certificate look like a complete one.
    want=$(openssl x509 -in "$CRT" -noout -enddate | cut -d= -f2)
    rc=0
    say ""
    for p in $PORT $PROXY_PORTS; do
        i=0
        got=""
        while [ "$i" -lt 15 ]; do
            got=$(served_expiry "$p" || true)
            [ "$got" = "$want" ] && break
            i=$((i + 1))
            sleep 2
        done
        if [ "$got" = "$want" ]; then
            say "  :$p  ✓ $got"
        else
            say "  :$p  ✗ ${got:-no answer}  (wanted $want)"
            rc=1
        fi
    done
    [ "$rc" = "0" ] && say "✓ deployed" || say "✗ deploy incomplete"
    return "$rc"
}

say "domain      $DOMAIN:$PORT"
for p in $PORT $PROXY_PORTS; do
    say "served :$p  $(served_expiry "$p" || echo unknown)"
done
[ -f "$CRT" ] && say "local til   $(openssl x509 -in "$CRT" -noout -enddate | cut -d= -f2)"

if [ "${CHECK:-0}" = "1" ]; then
    say "CHECK=1 — nothing changed."
    exit 0
fi

# DEPLOY_ONLY=1 pushes whatever is already in $LEGO_PATH without talking to the
# CA. Needed because renewal and deployment can fail independently: if the copy
# breaks after a successful renewal, a plain re-run finds the cert no longer due
# and reports "unchanged", skipping the very step that failed.
if [ "${DEPLOY_ONLY:-0}" = "1" ]; then
    say "DEPLOY_ONLY=1 — deploying the existing certificate, no CA contact."
    deploy
    exit 0
fi

command -v lego >/dev/null 2>&1 || { say "✗ lego not on PATH (brew install lego)"; exit 1; }

CF_TOKEN=$(security find-generic-password -s "$KEYCHAIN_SERVICE" -w 2>/dev/null || true)
if [ -z "$CF_TOKEN" ]; then
    say "✗ no Cloudflare token in the keychain under '$KEYCHAIN_SERVICE'."
    say "  Store it with:"
    say "    security add-generic-password -a \"\$USER\" -s $KEYCHAIN_SERVICE -w '<token>' -U"
    exit 1
fi
export CLOUDFLARE_DNS_API_TOKEN="$CF_TOKEN"

# Fingerprint before/after tells us whether anything actually changed, so a
# weekly no-op run does not restart nginx 51 times a year for nothing.
before=""
[ -f "$CRT" ] && before=$(openssl x509 -in "$CRT" -noout -fingerprint -sha256)

say ""
say "renewing (renews when under $RENEW_DAYS days remain)…"
# lego 5.x CLI, which is NOT the 4.x one most recipes online show:
#   - `renew` is gone; `run` gets-or-renews.
#   - `--path` is a `run` flag, not a global one. As a global it errors with
#     "flag provided but not defined: -path".
#   - `--days` became `--renew-days`, `--force` became `--renew-force`.
#
# --key-type RSA2048 matches the cert already deployed. v5 defaults to EC256,
# and a renewal during an outage is the wrong moment to also change key type.
#
# --no-random-sleep: lego otherwise sleeps a random interval before renewing, to
# spread load across the many clients hitting the CA on a shared schedule. This
# is one certificate on one host, and the sleep is time spent serving an expired
# cert, so the trade goes the other way here.
set -- run --path "$LEGO_PATH" --email "$EMAIL" --dns cloudflare \
       --domains "$DOMAIN" --key-type RSA2048 --accept-tos \
       --renew-days "$RENEW_DAYS" --no-random-sleep
[ "${FORCE:-0}" = "1" ] && set -- "$@" --renew-force
lego "$@"

after=$(openssl x509 -in "$CRT" -noout -fingerprint -sha256)
if [ "$before" = "$after" ]; then
    say "certificate unchanged — not redeploying."
    exit 0
fi

deploy

say ""
say "Force-kill and relaunch the iOS app: it is holding a socket to a server"
say "whose certificate just changed."
