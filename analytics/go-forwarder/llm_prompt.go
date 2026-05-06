// System-prompt loader for the AI session-analysis path
// (epic #412, issue #418).
//
// Prompts live as markdown files under analytics/go-forwarder/prompts/
// and are mounted into the image at /config/prompts/. Each prompt has
// optional YAML frontmatter for default model params + a version
// label that flows into the llm_calls ledger so prompt iterations
// correlate with output quality over time.
//
// Loaded at request time with mtime caching: re-read only when the
// file changes on disk. This lets operators iterate on a prompt with
// a kubectl rollout / docker compose reload of the config volume —
// no forwarder restart needed. Falls back to a built-in stub when
// the file is missing so AI features keep working out of the box.

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// PromptFrontmatter is the YAML block at the top of a prompt file.
// Frontmatter is delimited by '---' lines per common markdown
// convention (Hugo / Jekyll / Obsidian / etc.).
type PromptFrontmatter struct {
	Version            string  `yaml:"prompt_version"`
	DefaultMaxTokens   int     `yaml:"default_max_tokens"`
	DefaultTemperature float32 `yaml:"default_temperature"`
}

type Prompt struct {
	Path        string
	Frontmatter PromptFrontmatter
	Body        string
	mtime       time.Time
}

type PromptCache struct {
	mu      sync.Mutex
	entries map[string]*Prompt
}

var defaultPromptCache = &PromptCache{entries: map[string]*Prompt{}}

// LoadPrompt returns the parsed prompt for the given path, re-reading
// only if the file's mtime has changed. nil + error when the file is
// missing or malformed; callers fall back to the built-in stub below.
func (c *PromptCache) Load(path string) (*Prompt, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	cached, ok := c.entries[path]
	c.mu.Unlock()
	if ok && cached.mtime.Equal(stat.ModTime()) {
		return cached, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := parsePrompt(string(body))
	if err != nil {
		return nil, fmt.Errorf("parse prompt %s: %w", path, err)
	}
	p.Path = path
	p.mtime = stat.ModTime()
	c.mu.Lock()
	c.entries[path] = p
	c.mu.Unlock()
	return p, nil
}

func parsePrompt(text string) (*Prompt, error) {
	p := &Prompt{}
	r := bufio.NewReader(strings.NewReader(text))
	first, err := r.ReadString('\n')
	if err != nil {
		// Empty or near-empty file → no frontmatter, body is the
		// whole text.
		p.Body = text
		return p, nil
	}
	if strings.TrimSpace(first) != "---" {
		// No frontmatter delimiter — entire text is body.
		p.Body = text
		return p, nil
	}
	// Read until the closing '---' line, accumulating frontmatter.
	var fm strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("frontmatter not terminated by '---'")
		}
		if strings.TrimSpace(line) == "---" {
			break
		}
		fm.WriteString(line)
	}
	if err := yaml.Unmarshal([]byte(fm.String()), &p.Frontmatter); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	body, _ := readRest(r)
	p.Body = body
	return p, nil
}

func readRest(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		chunk, err := r.ReadString('\n')
		b.WriteString(chunk)
		if err != nil {
			return strings.TrimLeft(b.String(), "\n"), nil
		}
	}
}

// builtinSessionChatPrompt is the stub used when the on-disk prompt
// is missing. It must be enough to make /api/session_chat work end-
// to-end so test harnesses don't depend on the file being present.
const builtinSessionChatPrompt = `You are an expert in adaptive video streaming and ClickHouse analytics. Use the query tool to read from infinite_streaming.session_snapshots and infinite_streaming.network_requests. Cite timestamps in mm:ss.ms form. If data is missing, say so explicitly.`

// SessionChatPrompt loads /config/prompts/session_chat.md (overridable
// via SESSION_CHAT_PROMPT_PATH) or falls back to the built-in stub.
// Returns the body and the version string for ledger correlation.
func SessionChatPrompt() (body, version string) {
	path := os.Getenv("SESSION_CHAT_PROMPT_PATH")
	if path == "" {
		path = "/config/prompts/session_chat.md"
	}
	p, err := defaultPromptCache.Load(path)
	if err != nil {
		return builtinSessionChatPrompt, "builtin-fallback"
	}
	v := p.Frontmatter.Version
	if v == "" {
		v = "unversioned"
	}
	return p.Body, v
}
