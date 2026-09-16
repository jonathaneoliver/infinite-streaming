#!/bin/sh
# Have the VENDORED demo tools drifted from the Encoder copies they came from?
#
#   tools/demo/check_pronounce_sync.sh          # report
#   tools/demo/check_pronounce_sync.sh --sync   # pull Encoder's copies over
#
# WHY THIS EXISTS: the README already said "keep it byte-identical", and it
# drifted anyway. pronounce.py fell three rules behind, one of which (":\s+" ->
# ", ") affected 70 of one take's 104 cues. These tools are learned by ear and
# by ruined take, one painful lesson at a time, and both projects narrate in the
# same voice about overlapping subject matter — so a fix made over there is
# almost always right over here. The cost of drift is silently re-learning
# something already solved.
#
# Covers every vendored file, not just pronounce.py: make_ass.py drifted too
# (captions had WrapStyle 2, so long lines ran off the frame instead of
# wrapping), which is exactly the class of bug this is meant to catch.
#
# Not a git hook and not wired into CI: the Encoder checkout is a local path
# that will not exist for anyone else. It is a preflight you run before a take,
# and the narration step calls it.
#
# Direction matters. Encoder is upstream — it has the larger narration corpus
# and usually gets the rules first. If a file here has lines Encoder lacks, that
# is a real divergence to resolve by hand (fix it upstream, then re-sync), so
# --sync refuses to clobber it.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
THEM="${ENCODER_DEMO:-$HOME/Projects/Encoder/tools/demo}"

# The vendored set, per the README's layout table. record_valley.js and
# render_layout.py are app-specific and deliberately absent.
FILES="pronounce.py make_ass.py narrate_sentences.py audition.py join.py edit_text.py narrator_app.py narrator.html"

if [ ! -d "$THEM" ]; then
    echo "vendor: no Encoder checkout at $THEM — skipping drift check" >&2
    exit 0
fi

drifted=0
ahead=0
for f in $FILES; do
    mine="$HERE/$f"
    theirs="$THEM/$f"
    [ -f "$mine" ] && [ -f "$theirs" ] || continue
    if diff -q "$theirs" "$mine" >/dev/null 2>&1; then
        continue
    fi
    only_mine=$(diff "$theirs" "$mine" | grep -c '^>' || true)
    only_theirs=$(diff "$theirs" "$mine" | grep -c '^<' || true)
    echo "vendor: DRIFTED $f — $only_theirs line(s) only in Encoder, $only_mine only here"
    drifted=$((drifted + 1))
    [ "$only_mine" -gt 0 ] && ahead=$((ahead + 1))

    if [ "${1:-}" = "--sync" ] && [ "$only_mine" -eq 0 ]; then
        cp "$theirs" "$mine"
        echo "  synced $f from Encoder"
        drifted=$((drifted - 1))
    fi
done

if [ "$drifted" -eq 0 ]; then
    echo "vendor: in sync with Encoder"
    exit 0
fi

if [ "$ahead" -gt 0 ]; then
    echo "  $ahead file(s) have lines Encoder lacks — merge by hand, upstream first." >&2
else
    echo "  adopt: $0 --sync"
fi
exit 1
