# Findings library

Per-discovery markdown files capturing what we've learned from real
investigations. Each file is the operator's narrative explanation of a
single observed behaviour, paired with a sibling JSON snapshot that
the `harness finding add` command wrote at the moment of capture.

Skills that read this library:

- `finding` — capture (writes new files) + recall (greps existing)
- `forensics` — the subagent reads files matching its current query
  before hypothesising, so we don't re-derive things we've already
  learned

## When to add a finding

The bar is: *will Future Us thank Past Us for writing this down?*

Add a finding when:
- A bug or non-obvious behaviour took >20 minutes to understand
- The cause turned out to be different from the initial hypothesis
- A workaround exists but the root cause isn't fixed yet
- A platform (iOS / Roku / Android TV) does something unusual that
  isn't in our standards docs

Don't add a finding for:
- Things already obvious from reading the code
- One-off transient blips with no understood cause
- Notes you'd put in a commit message instead

## Format

```markdown
# <Symptom> — <player short id> — <YYYY-MM-DD>

## Summary
1–3 sentences. What broke; what we now know; what the operator should
do about it (or "needs more investigation").

## Timeline
- T+0:00  thing happened
- T+0:23  next thing
- T+1:14  …

## Evidence
Inline `harness` command outputs or jq excerpts that justify the
timeline. If the JSON snapshot file (`harness finding add`'s output)
has the raw data, link it: `see ee091d13-1779057782294.json`.

## Hypothesis
One paragraph. Tag explicitly: **confirmed** | **refuted** |
**needs-test**.

## Action items
- [ ] Reproduce with `harness procedure …`
- [ ] File issue if confirmed bug
- [ ] Update standards doc if this teaches a new platform fact
```

## Filename convention

`<player-shortid>-<symptom>-<YYYY-MM-DD>.md` — e.g.
`ee091d13-buffer-collapse-2026-05-17.md`. The shortid + date is what
the `finding` skill greps against when surfacing related history.

## Sibling JSON

`harness finding add` writes a JSON file named
`<shortid>-<unix-ms>.json` containing the full PlayerRecord snapshot,
recent mutation history, tags, and free-form note at the moment of
capture. The `.md` file here is the narrative; the `.json` is the data.
Link the two in the Evidence section.
