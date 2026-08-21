#!/bin/sh
# Has pronounce.py drifted from the Encoder copy it is vendored from?
#
#   tools/demo/check_pronounce_sync.sh          # report
#   tools/demo/check_pronounce_sync.sh --sync   # pull Encoder's copy over
#
# WHY THIS EXISTS: the README already said "keep it byte-identical", and it
# drifted anyway — this copy fell three rules behind, one of which (":\s+" ->
# ", ") affected 70 of one take's 104 cues. Pronunciation is learned by ear, one
# painful take at a time, and both projects narrate in the same voice about
# overlapping subject matter. A rule learned over there is almost always right
# over here, so the cost of drift is silently re-learning something already
# solved.
#
# Not a git hook and not wired into CI: the Encoder checkout is a local path
# that will not exist for anyone else. It is a preflight you run before a take,
# and the narration step calls it.
#
# Direction matters. Encoder is upstream — it has the larger narration corpus
# and gets the rules first. If this copy has lines Encoder lacks, that is a real
# divergence to resolve by hand, not something to overwrite, so --sync refuses.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
MINE="$HERE/pronounce.py"
THEIRS="${ENCODER_DEMO:-$HOME/Projects/Encoder/tools/demo}/pronounce.py"

if [ ! -f "$THEIRS" ]; then
    echo "pronounce: no Encoder checkout at $THEIRS — skipping drift check" >&2
    exit 0
fi

if diff -q "$THEIRS" "$MINE" >/dev/null 2>&1; then
    echo "pronounce: in sync with Encoder"
    exit 0
fi

ONLY_MINE=$(diff "$THEIRS" "$MINE" | grep -c '^>' || true)
ONLY_THEIRS=$(diff "$THEIRS" "$MINE" | grep -c '^<' || true)

echo "pronounce: DRIFTED — $ONLY_THEIRS line(s) only in Encoder, $ONLY_MINE only here"

if [ "${1:-}" = "--sync" ]; then
    if [ "$ONLY_MINE" -gt 0 ]; then
        echo "  refusing to sync: this copy has $ONLY_MINE line(s) Encoder lacks." >&2
        echo "  That is a real divergence — merge it by hand, upstream first." >&2
        exit 1
    fi
    cp "$THEIRS" "$MINE"
    echo "  synced from $THEIRS"
    exit 0
fi

echo "  review:  diff $THEIRS $MINE"
echo "  adopt:   $0 --sync"
exit 1
