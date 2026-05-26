package main

// llm_chat_persistence.go — write each chat turn to a markdown
// file under <claudeDir>/chats/<chat_id>.md so the conversation
// (including raw tool args + results) can be handed off to
// Claude Code, grepped, archived, or diffed without going back
// to the SSE wire history that the browser only has a summary of.
//
// File lifecycle:
//   - First turn: create file with header (chat_id, scope, model)
//   - Subsequent turns: append a block (## Turn N) with the new
//     user message + assistant text + tool calls/results + usage
//   - File is never deleted by the forwarder; ops can prune by mtime
//
// One file per chat_id. Concurrent turns for the same chat_id
// don't happen (the UI is serial per panel), but the write is
// atomic at the OS level — each append is a single os.OpenFile +
// Write call with O_APPEND.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PersistChatTurnInput is everything one turn needs to render to
// markdown. Built once per turn at the end of streamChat.
type PersistChatTurnInput struct {
	ChatID    string
	Scope     ChatScope
	Profile   string
	Model     string
	BaseURL   string
	// PriorMessages is everything in the client-supplied history
	// EXCEPT the new user turn. Used only when the chat file
	// doesn't exist yet — written under "Earlier history" so a
	// chat that started before persistence came online (or before
	// the chats/ dir was writable) still has the operator's
	// browser-localStorage context captured the moment persistence
	// recovers. Ignored on subsequent turns.
	PriorMessages []LLMMessage
	NewMessages []LLMMessage // user turn + assistant/tool messages produced this turn
	Usage     LLMUsage
	CostUSD   float64
	DurationMs uint32
	ToolCallsCount int
	Status    string
	ErrorKind string
}

// PersistChatTurn appends one turn's worth of markdown to the
// chat's file. Best-effort: errors are logged but never fail the
// chat response (persistence is a convenience, not a correctness
// requirement). When claudeDir is unset, the function returns the
// path it would have written but does no I/O.
func PersistChatTurn(claudeDir string, in PersistChatTurnInput) (string, error) {
	if claudeDir == "" || claudeDir == "/dev/null/.claude" {
		// Forwarder running without a real claude mount — silent skip.
		return "", nil
	}
	dir := filepath.Join(claudeDir, "chats")
	if err := osMkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir chats: %w", err)
	}
	path := filepath.Join(dir, sanitiseChatID(in.ChatID)+".md")

	first := false
	if _, err := osStat(path); err != nil && os.IsNotExist(err) {
		first = true
	}

	var buf strings.Builder
	if first {
		writeChatHeader(&buf, in)
		if len(in.PriorMessages) > 0 {
			writePriorHistoryBlock(&buf, in.PriorMessages)
		}
	}
	writeTurnBlock(&buf, in, first)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return path, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(buf.String()); err != nil {
		return path, fmt.Errorf("write: %w", err)
	}
	return path, nil
}

// writePriorHistoryBlock dumps the client-supplied earlier turns
// under a clearly-labelled section so a reader (operator or
// Claude Code via handoff) knows these came from browser
// localStorage rather than from the server's per-turn capture.
// Tool messages here carry the FULL content the bot saw — the
// client always sends the wire history verbatim — so this
// backfill is faithful even though it's not append-as-you-go.
func writePriorHistoryBlock(buf *strings.Builder, prior []LLMMessage) {
	buf.WriteString("---\n\n## Earlier history (pre-persistence backfill)\n\n")
	buf.WriteString("> These turns happened before this chat file existed. " +
		"They were carried in the client's localStorage and captured on the " +
		"first successful persist. Anything before this includes only the wire " +
		"history, not the side-channel SSE summaries the operator saw.\n\n")
	for _, m := range prior {
		writeMessage(buf, m)
	}
}

func writeChatHeader(buf *strings.Builder, in PersistChatTurnInput) {
	fmt.Fprintf(buf, "# Chat %s\n\n", in.ChatID)
	fmt.Fprintf(buf, "**Created:** %s  \n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(buf, "**Profile:** %s · **Model:** %s  \n", in.Profile, in.Model)
	if in.BaseURL != "" {
		fmt.Fprintf(buf, "**Upstream:** %s  \n", in.BaseURL)
	}
	if s := scopeOneLine(in.Scope); s != "" {
		fmt.Fprintf(buf, "**Scope:** %s  \n", s)
	}
	buf.WriteString("\n> This file is auto-appended by the forwarder on every turn.\n")
	buf.WriteString("> Hand off to Claude Code with: `cat <path-to-this-file>` then ask it to continue.\n\n")
}

// turnCounter is non-authoritative — each append re-derives the
// turn number by counting `## Turn` markers already on disk would
// be the only authoritative way, but appending the timestamp +
// "Turn (continued)" tag suffices for the human reader. Keep it
// simple: just timestamp each block.
func writeTurnBlock(buf *strings.Builder, in PersistChatTurnInput, isFirst bool) {
	if !isFirst {
		buf.WriteString("\n---\n\n")
	}
	fmt.Fprintf(buf, "## Turn — %s\n\n", time.Now().UTC().Format(time.RFC3339))
	if s := scopeOneLine(in.Scope); s != "" && !isFirst {
		// Repeat scope per-turn — the dashboard's brush/expanded
		// card can change between turns, and the operator handing
		// off may only have caught the file mid-conversation.
		fmt.Fprintf(buf, "**Scope:** %s  \n\n", s)
	}
	for _, m := range in.NewMessages {
		writeMessage(buf, m)
	}
	fmt.Fprintf(buf, "\n**Usage:** %d in / %d out · $%.4f · %dms · %d tool calls",
		in.Usage.InputTokens, in.Usage.OutputTokens, in.CostUSD,
		in.DurationMs, in.ToolCallsCount)
	if in.Status != "" && in.Status != LLMStatusOK {
		fmt.Fprintf(buf, " · status=%s", in.Status)
		if in.ErrorKind != "" {
			fmt.Fprintf(buf, " err=%s", in.ErrorKind)
		}
	}
	buf.WriteString("\n")
}

func writeMessage(buf *strings.Builder, m LLMMessage) {
	switch m.Role {
	case "user":
		buf.WriteString("### User\n\n")
		buf.WriteString(strings.TrimSpace(m.Content))
		buf.WriteString("\n\n")
	case "assistant":
		buf.WriteString("### Assistant\n\n")
		if txt := strings.TrimSpace(m.Content); txt != "" {
			buf.WriteString(txt)
			buf.WriteString("\n\n")
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(buf, "**Tool call:** `%s` (`%s`)\n\n", tc.Function.Name, tc.ID)
			args := strings.TrimSpace(tc.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			buf.WriteString("```json\n")
			buf.WriteString(prettyJSONOrRaw(args))
			buf.WriteString("\n```\n\n")
		}
	case "tool":
		fmt.Fprintf(buf, "**Tool result:** `%s` → `%s`\n\n", m.Name, m.ToolCallID)
		buf.WriteString("```json\n")
		buf.WriteString(prettyJSONOrRaw(m.Content))
		buf.WriteString("\n```\n\n")
	}
}

// scopeOneLine renders the scope as a single-line summary for the
// header and per-turn line.
func scopeOneLine(s ChatScope) string {
	if s.Kind == "" {
		return ""
	}
	parts := []string{"kind=" + s.Kind}
	if s.PlayerID != "" {
		parts = append(parts, "player_id="+s.PlayerID)
	}
	if s.PlayID != "" {
		parts = append(parts, "play_id="+s.PlayID)
	}
	if s.From != "" {
		parts = append(parts, "from="+s.From)
	}
	if s.To != "" {
		parts = append(parts, "to="+s.To)
	}
	if s.RunID != "" {
		parts = append(parts, "run_id="+s.RunID)
	}
	if s.Cycle != 0 {
		parts = append(parts, fmt.Sprintf("cycle=%d", s.Cycle))
	}
	return strings.Join(parts, " ")
}

// prettyJSONOrRaw pretty-prints if input parses; otherwise returns
// the raw string untouched. Keeps the file readable but resilient
// to non-JSON tool results (e.g. plain-text errors).
func prettyJSONOrRaw(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(out)
}

// sanitiseChatID restricts the chat_id to filesystem-safe chars.
// The newID() generator produces hex so this is paranoid; defends
// against future format changes.
func sanitiseChatID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	if b.Len() > 64 {
		return b.String()[:64]
	}
	return b.String()
}
