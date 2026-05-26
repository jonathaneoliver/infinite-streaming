---
name: handoff
description: Resume an investigation that started in the dashboard's AI chat panel. The user pastes a `cat ~/test-dev/.claude/chats/<id>.md` command (or a URL); read the file, understand what scope was set and which tools were already called, then continue the investigation with full Claude Code capabilities (shell, edit, full reasoning budget). Use when the user references a "chat handoff" or pastes one of those commands without further explanation.
last_reviewed: 2026-05-24
---

# Handoff — pick up a dashboard chat investigation

The dashboard's AI chat panel (#497) is intentionally constrained: limited context window, restricted tool surface, no shell, no code edits. Operators use it for fast triage at the data — "what's wrong with this play", "summarise this run" — and **hand off to Claude Code** when they need depth: writing a fix, comparing across many sessions, reading source.

Every chat is auto-persisted to `<claudeDir>/chats/<chat_id>.md`. The handoff button in the chat header emits a `cat` command and an HTTP URL — both fetch the same file.

## When this skill applies

The user pasted something like:

```
cat ~/test-dev/.claude/chats/a1b2c3d4e5f6.md
```

or:

```
https://.../analytics/api/v2/chat/handoffs/a1b2c3d4e5f6
```

…with little or no other context. They're asking you to read it and continue.

## Flow

### 1. Read the chat

Run the command they pasted. If it's a `cat`, it's a local read. If it's a URL, use `curl -s`. If neither works (e.g. host unreachable), ask whether the file is on a different box and offer to `scp` it.

### 2. Extract the scope

The file's header has the canonical scope the panel was in: `kind=play player_id=... play_id=...` or `kind=range from=... to=...` or `kind=characterization run_id=... test_name=...`. **State the scope back to the user in one line** so they know you correctly picked up where they were.

### 3. Read the tool-call trail

Each turn lists the tools the bot called and their raw JSON results. **Don't re-run them blindly** — those results are already in the file. Treat the bot's tool calls as evidence already gathered. Re-run only when:

- The data is stale (results > 30 min old AND the investigation depends on current state)
- The bot's result was an error or truncated
- You need a different cut (different filter, different time window)

### 4. Continue, don't restart

Read the last user message + the bot's last response. The user's open question is almost always the **next thing the bot was about to address but couldn't** — context budget exhausted, tool returned an error, or the answer needed deeper reasoning.

Common patterns:

| Last bot state | What to do |
|---|---|
| Found a labelled play, suggested investigating | Run forensics (or `investigate`) on that play |
| Compared two runs, surfaced a delta | Read source code, find when the regression landed |
| Proposed a finding but operator hadn't saved | Validate the finding before saving; tighten the prose |
| Hit `tool_budget` error mid-investigation | Continue the tool sequence the bot was running |
| Said "subagent hit X" | The dashboard's subagent is restricted; you have full access — just do it |

### 5. Write findings back

If you reach a tagged conclusion, write it to `.claude/findings/<slug>.md` following the same shape as existing findings. The chat panel's `list_findings` / `read_finding` tools will surface it on the next browser session — investigation closes the loop.

## What you have that the chat panel doesn't

- Full shell (`Bash`): `harness query`, `curl`, `ssh $TEST_SSH`, `make`, `git`
- Edit + Write: change source, add tests, fix bugs
- Larger context + memory recall (`.claude/memory/`)
- Subagents (`Agent` tool): forensics-expert, code-reviewer, general-purpose
- No tool budget; no $5/day cap; the user already paid for you

So when the chat panel said "this is beyond my scope" or "the subagent failed", you can almost always just do it.

## What you should NOT do

- Re-derive evidence the bot already has in the file — it's there, read it
- Erase or modify the chat file — it's an audit record; append a finding instead
- Start a new chat in the dashboard from the CLI; just continue here
- Echo the entire chat file back to the user — they wrote it; summarise instead
