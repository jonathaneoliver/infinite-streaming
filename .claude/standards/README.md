# Standards library

One-page operationally-relevant cheat sheets per topic. Read by the
`forensics` subagent before it hypothesises, and by Claude generally
when investigating a topic mid-session.

## The bar

If I'm reasoning about this topic mid-investigation, **what 3 facts
would I want quoted to me?** Those facts go in. Anything else doesn't.

Not specs. Not exhaustive. Not aspirational. These are:
- Things that are true in the real world *and* relevant to debugging
  this product
- Especially things that have already bitten us at least once
- Stated tersely, with concrete platform names where it matters
  (AVPlayer, ExoPlayer, Roku Stream Player, hls.js, Shaka)

## Format

```markdown
# <Topic>

One-sentence framing.

## <Fact category 1>
- Fact. Why it matters. Where we hit it.
- Fact. …

## <Fact category 2>
- …

## Common mistakes
- …

## See also
- Other standards / findings to read alongside.
```

Length: one page rendered. If it grows past that, split.

## Current entries

- `hls-taxonomy.md` — m3u8 tags, what each means, what the proxy
  emits/strips
- `avplayer-quirks.md` — iOS-specific behaviours (init cache,
  transfer_abandoned, buffer metric reporting gaps)
- `abr-decision-model.md` — how a player chooses variants, why
  downshift cascades, what triggers timejump
- `codec-strings.md` — avc1/hev1/mp4a profile-level-tier, what
  platforms require what, common stripping bugs
- `invariants.md` — operating manual for the aberration-crawl rule
  catalogue (`tests/aberration_crawl/invariants.yaml`): validity
  windows, census-before-assert, documented NON-rules

## When to add a new one

When an investigation kept getting stuck because we didn't know a
basic fact about a platform / protocol / behaviour, and that fact
took >10 min to confirm. Write the 3 facts down so the next session
doesn't pay the cost.
