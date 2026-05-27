---
name: finding
description: Capture a discovery into the .claude/findings/ library, or recall existing findings related to the current investigation. Invoke when the user says "save that", "record this finding", "this is worth remembering", "what do we know about X", "have we seen this before", or after a successful `investigate` / `forensics` run that reached a tagged hypothesis. The library is the project's memory across sessions — write to it when a non-obvious cause has been confirmed or strongly suspected.
last_reviewed: 2026-05-19
---

# Findings library — capture + recall

The library lives at `.claude/findings/` (per-repo, committed). Each finding is a markdown narrative + (usually) a JSON sidecar with the raw player state at the moment of capture. See `.claude/findings/README.md` for the file format and the bar for adding a finding.

**Conventions:** this skill follows `.claude/skills/CONVENTIONS.md`. Most load-bearing for finding: durable storage stays UTC (the sidecar JSON, file names, and the narrative's timestamps); only the rendered display lines you show the operator get converted to local time.

## When to invoke

**Capture mode** — triggered on:
- "save that", "record the finding", "this is worth remembering"
- After `investigate` reaches a `confirmed` or strongly-suspected `needs-test` hypothesis
- When the user describes a discovery in chat ("turns out AVPlayer caches the init") and it's not in the library

**Recall mode** — triggered on:
- "what do we know about X"
- "have we seen this before"
- "search findings for stalls / AVPlayer / 2160p / etc"
- Implicitly: before any `forensics` dispatch (always grep findings first)

## Capture flow

### 1. Get the JSON sidecar via the CLI

If the finding is about a live or recently-active player, call:

```sh
harness --insecure finding add <target> \
  --tag <tag1> --tag <tag2> \
  --note "<one-line operator note>"
```

The CLI writes `.claude/findings/<shortid>-<unix-ms>.json` containing the full PlayerRecord at this moment + recent mutation snapshots. The note becomes the seed for the markdown narrative.

If the finding has no live target (e.g. "Apple TV `tvOS focus` requires `cinematicFocusFollower` inside Button labels"), skip this step — capture is markdown-only.

### 2. Write the narrative `.md` sibling

Filename: `<player-shortid>-<symptom>-<YYYY-MM-DD>.md` — e.g. `ee091d13-buffer-collapse-2026-05-17.md`. If no player is involved, use `<topic>-<YYYY-MM-DD>.md` — e.g. `avplayer-init-cache-2026-05-17.md`.

Template (also in `.claude/findings/README.md`):

```markdown
# <Symptom> — <player short id> — <YYYY-MM-DD>

## Summary
1–3 sentences. What broke; what we now know; what the operator should
do about it (or "needs more investigation").

## Timeline
- T+0:00  thing happened
- T+0:23  next thing

## Evidence
Inline harness output. Link the JSON sidecar:
> see `ee091d13-1779057782294.json` for the full PlayerRecord at
> capture time.

## Hypothesis
One paragraph. Tag: **confirmed** | **refuted** | **needs-test**.

## Action items
- [ ] Reproduce with `harness procedure …`
- [ ] File issue if confirmed bug
- [ ] Update standards doc if this teaches a new platform fact
```

Write it dense. The reader 3 months from now wants the headline in the first sentence and the JSON sidecar for full data.

### 3. Suggest related cross-references

After writing, grep for related findings:

```sh
grep -li <symptom-keyword> .claude/findings/*.md
```

If matches exist, mention them in the new finding's `## See also` section. The library is more useful as a graph than as a list.

### 4. If this teaches a NEW platform fact, suggest a standards update

If the cause is a platform behaviour we didn't previously know about (e.g. "iOS abandons in-flight transfers after 20s regardless of buffer state"), suggest adding 1–2 lines to the matching `.claude/standards/` doc. The finding records the *incident*; the standards doc records the *general fact*.

## Recall flow

### 1. Grep first, hypothesise later

```sh
grep -ril <keyword> .claude/findings/
```

Use multiple keyword passes if you're searching for a concept:
- For "iPad stall": `grep -ril ipad .claude/findings/` + `grep -ril stall .claude/findings/` then intersect
- For "AVPlayer cache": `grep -ril avplayer .claude/findings/` + `grep -ril cache .claude/findings/`

### 2. Surface the 1–3 most-relevant

Don't dump the whole grep output. Pick the 1-3 most-relevant by filename + summary. Cite them by filename, quote the Summary section, link to the sidecar JSON if useful.

### 3. State whether the prior finding answers the current question

The recall isn't done when you've found the files — it's done when you've said "yes, [filename] explains this — proceed as it suggests" or "no, this is similar to [filename] but the trigger differs in X way, so we still need to investigate".

## Templates for common capture cases

### Single-event investigation that reached a hypothesis

The `investigate` skill ended with a tagged hypothesis. Capture is mechanical:
1. `harness finding add <t> --tag stall --note "<the hypothesis 1-liner>"`
2. Copy the `investigate` output's Timeline + Evidence sections into the new `.md`
3. Verbatim-copy the Hypothesis paragraph (don't paraphrase — the original wording is the record)

### Platform/protocol fact discovered while debugging

Skip step 1 (no JSON sidecar needed). Just write the `.md`:
- `# Topic — YYYY-MM-DD`
- `## Summary` = the fact in one sentence
- `## How we learned this` = the bug or investigation that led here
- `## Where this matters` = which player platforms / scenarios

Then propose adding the fact to `.claude/standards/<topic>.md`.

### Reproducibility recipe

When a `forensics` test confirms reproduction:
- Include the EXACT `harness procedure` / `harness fault add` / `harness shape` command that reproduces, with all flags
- Note expected vs actual behaviour
- Tag `confirmed` (since reproduction proves cause)

## What NOT to do here

- **Don't capture findings for transient blips with no understood cause.** The library's value is signal:noise. A finding that says "ipad stalled, dunno why" pollutes future searches.
- **Don't paraphrase the hypothesis when copying from `investigate`.** Verbatim — the original wording is the record.
- **Don't capture without checking the library first.** Duplicates make the library worse, not better.
- **Don't recall by trying to guess the finding's filename.** Grep first.

## See also

- `.claude/findings/README.md` — format spec + when-to-add bar
- `.claude/standards/README.md` — when a finding teaches a general fact, update the standards
- `investigate` — the typical upstream that feeds capture
- `forensics` — recall is always invoked before forensics dispatches to the subagent
