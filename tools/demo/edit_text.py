#!/usr/bin/env python3
"""Move the narration between cues.json, a scratch edit file, and git.

Editing prose inside JSON is miserable and easy to corrupt — one missing quote
and the whole pipeline stops. Everything here exports one line per cue keyed by
INDEX, so reordering or rewrapping a file cannot silently attach text to the
wrong cue.

  python3 edit_text.py export   -> $DEMO_DIR/narration-edit.txt  (with timings)
  python3 edit_text.py apply    <- read that back into cues.json

  python3 edit_text.py save     -> tools/demo/narrative/<project>.txt  (tracked)
  python3 edit_text.py load     <- read the tracked narrative into cues.json

## Why two pairs

`export`/`apply` is the scratch loop while you are working against one
recording. Each line carries its timestamp so you can find it in the video.

`save`/`load` is the version-controlled one, and it deliberately **omits the
timestamps**. The narrative is authored; the timings are measured, and they move
a little on every take. Track them together and re-recording the same script
rewrites every line of the file — the diff then shows 60 changed lines when
nothing was said differently, and the history stops being readable exactly when
you want to ask "what did this used to say?".

So the split is: text in git, timings in cues.json, joined by index.

That also means `load` refuses a cue-count mismatch. A narrative written against
a 60-cue recording applied to a 58-cue one would silently shift every line after
the gap onto the wrong moment — the failure is invisible until playback.
"""
import json, os, re, shutil, sys

HERE = os.path.dirname(os.path.abspath(__file__))
DEMO_DIR = os.environ.get("DEMO_DIR", os.path.expanduser("~/Desktop/encoder-demo"))
CUES = os.environ.get("CUES", os.path.join(DEMO_DIR, "cues.json"))
TXT = os.environ.get("TXT", os.path.join(DEMO_DIR, "narration-edit.txt"))

# One narrative per project, so a second app using this tooling adds a file
# rather than overwriting this one.
PROJECT = os.environ.get("DEMO_PROJECT", "encoder")
NARRATIVE_DIR = os.environ.get("NARRATIVE_DIR", os.path.join(HERE, "narrative"))

HEADER = """# NARRATION — edit the text after each timestamp, then run:
#     python3 edit_text.py apply
#
# The [NN] index is how a line is matched back; do not change or reorder it.
# The timestamp is where the line appears in the video (informational).
# Lines starting with # are ignored. One cue per line.
#
# What you write here becomes BOTH the on-screen caption and the spoken
# narration — abbreviations are expanded for the voice automatically
# (4K -> "four K", VBV -> "V B V"), so write it as you want it READ.
"""

NARRATIVE_HEADER = """# NARRATIVE — {project}
#
# This file is TRACKED IN GIT. It is the script; the recording supplies the
# timings. Edit here, commit, and `git diff` shows what changed about what is
# SAID rather than what changed about when it was said.
#
#     python3 edit_text.py load     apply this into cues.json
#     python3 edit_text.py save     write cues.json back out to here
#
# The [NN] index is how a line is matched back — do not renumber or reorder.
# There are deliberately no timestamps here; see the module docstring.
# Blank lines and # comments are ignored. One cue per line.
#
# Text is expanded for the voice automatically (4K -> "four K"), so write it
# as you want it READ.
"""


def narrative_path(project=None):
    return os.path.join(NARRATIVE_DIR, "%s.txt" % (project or PROJECT))


# Caption text may contain newlines — the recorder uses them to break a long
# caption across two visual lines, and 7 of the 42 cues in the first demo do.
# Written raw, those become extra file lines that parse as neither a cue nor a
# comment, so the line-per-cue contract silently breaks and the reader either
# drops them or mis-indexes everything after. Escaped on the way out, restored
# on the way back in.
def _esc(t):
    return t.replace("\\", "\\\\").replace("\n", "\\n")


def _unesc(t):
    out, i = [], 0
    while i < len(t):
        if t[i] == "\\" and i + 1 < len(t):
            nxt = t[i + 1]
            if nxt == "n":
                out.append("\n"); i += 2; continue
            if nxt == "\\":
                out.append("\\"); i += 2; continue
        out.append(t[i]); i += 1
    return "".join(out)


def mmss(s):
    return "%02d:%02d" % (int(s // 60), int(s % 60))


def _read_indexed(path, pattern):
    """Parse `[NN] …` lines into {index: text}. Unparseable lines are fatal."""
    edits, bad = {}, []
    with open(path) as f:
        for ln in f:
            ln = ln.rstrip("\n")
            if not ln.strip() or ln.lstrip().startswith("#"):
                continue
            m = re.match(pattern, ln)
            if not m:
                bad.append(ln[:60]); continue
            edits[int(m.group(1))] = _unesc(m.group(2).strip())
    return edits, bad


def _apply_edits(edits, label):
    data = json.load(open(CUES))
    cues = data["cues"]
    changed = 0
    for i, new in sorted(edits.items()):
        if i >= len(cues):
            print("  cue %d does not exist — skipped" % i); continue
        if cues[i]["text"] != new:
            print("  [%02d] %r\n    -> %r" % (i, cues[i]["text"][:56], new[:56]))
            cues[i]["text"] = new
            cues[i].pop("spoken", None)      # derived at generation time now
            changed += 1
    if not changed:
        print("  no changes"); return 0
    shutil.copy(CUES, CUES + ".bak")
    json.dump(data, open(CUES, "w"), indent=2)
    print("  %d cue(s) updated from %s (backup at %s.bak)"
          % (changed, label, os.path.basename(CUES)))
    print("  next:  python3 make_ass.py  &&  python3 narrate_sentences.py")
    return 0


def export():
    cues = json.load(open(CUES))["cues"]
    with open(TXT, "w") as f:
        f.write(HEADER + "\n")
        for i, c in enumerate(cues):
            f.write("[%02d] %s  %s\n" % (i, mmss(c["at"]), _esc(c["text"])))
    print("wrote %s — %d lines" % (TXT, len(cues)))


def apply():
    edits, bad = _read_indexed(TXT, r"^\[(\d+)\]\s+\d+:\d+\s+(.*)$")
    if bad:
        print("  %d unparseable line(s) IGNORED — nothing applied:" % len(bad))
        for b in bad:
            print("    %s" % b)
        return 1
    return _apply_edits(edits, os.path.basename(TXT))


def save(project=None):
    """cues.json -> the tracked narrative (no timestamps)."""
    cues = json.load(open(CUES))["cues"]
    path = narrative_path(project)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(NARRATIVE_HEADER.format(project=project or PROJECT) + "\n")
        for i, c in enumerate(cues):
            f.write("[%02d] %s\n" % (i, _esc(c["text"])))
    print("wrote %s — %d lines" % (path, len(cues)))
    print("  commit it:  git add %s" % os.path.relpath(path))
    return 0


def load(project=None):
    """The tracked narrative -> cues.json."""
    path = narrative_path(project)
    if not os.path.exists(path):
        print("  no narrative at %s" % path)
        print("  record first, then:  python3 edit_text.py save")
        return 1
    edits, bad = _read_indexed(path, r"^\[(\d+)\]\s+(.*)$")
    if bad:
        print("  %d unparseable line(s) — nothing applied:" % len(bad))
        for b in bad:
            print("    %s" % b)
        return 1
    n = len(json.load(open(CUES))["cues"])
    if len(edits) != n:
        print("  REFUSING: narrative has %d lines, this recording has %d cues."
              % (len(edits), n))
        print("  Lines are matched by index, so a mismatch shifts the script onto")
        print("  the wrong moments and you would only find out on playback.")
        print("  If the drive changed, re-save the narrative and re-apply your edits.")
        return 2
    return _apply_edits(edits, os.path.relpath(path))


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "export"
    proj = sys.argv[2] if len(sys.argv) > 2 else None
    if cmd == "apply":
        sys.exit(apply())
    if cmd == "save":
        sys.exit(save(proj))
    if cmd == "load":
        sys.exit(load(proj))
    sys.exit(export() or 0)
