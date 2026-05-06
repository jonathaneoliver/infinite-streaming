package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePromptTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "session_chat.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write tmp prompt: %v", err)
	}
	return p
}

func TestParsePrompt_NoFrontmatter(t *testing.T) {
	p, err := parsePrompt("just a plain prompt body\nno frontmatter\n")
	if err != nil {
		t.Fatalf("parsePrompt: %v", err)
	}
	if !strings.Contains(p.Body, "plain prompt body") {
		t.Errorf("Body = %q", p.Body)
	}
	if p.Frontmatter.Version != "" {
		t.Errorf("expected empty version when no frontmatter")
	}
}

func TestParsePrompt_WithFrontmatter(t *testing.T) {
	src := `---
prompt_version: v3
default_max_tokens: 8000
default_temperature: 0.5
---
the prompt body
goes here
`
	p, err := parsePrompt(src)
	if err != nil {
		t.Fatalf("parsePrompt: %v", err)
	}
	if p.Frontmatter.Version != "v3" {
		t.Errorf("Version = %q, want v3", p.Frontmatter.Version)
	}
	if p.Frontmatter.DefaultMaxTokens != 8000 {
		t.Errorf("DefaultMaxTokens = %d, want 8000", p.Frontmatter.DefaultMaxTokens)
	}
	if !strings.HasPrefix(p.Body, "the prompt body") {
		t.Errorf("Body = %q (frontmatter not stripped?)", p.Body)
	}
}

func TestParsePrompt_UnterminatedFrontmatter(t *testing.T) {
	_, err := parsePrompt("---\nprompt_version: v1\nbody never reached")
	if err == nil {
		t.Fatal("expected error for unterminated frontmatter")
	}
}

func TestPromptCache_ReReadOnMtimeChange(t *testing.T) {
	cache := &PromptCache{entries: map[string]*Prompt{}}
	path := writePromptTemp(t, "first body\n")
	first, err := cache.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(first.Body, "first") {
		t.Fatalf("Body = %q", first.Body)
	}
	// Bump mtime by sleeping briefly (filesystem mtime resolution is
	// sometimes 1 s on older FSes; 1.1 s is safe everywhere we run).
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(path, []byte("second body\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	second, err := cache.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(second.Body, "second") {
		t.Errorf("cache didn't refresh on mtime change; got %q", second.Body)
	}
}

func TestPromptCache_HitsCache(t *testing.T) {
	cache := &PromptCache{entries: map[string]*Prompt{}}
	path := writePromptTemp(t, "stable body\n")
	a, _ := cache.Load(path)
	b, _ := cache.Load(path)
	if a != b {
		t.Errorf("cache should return same pointer for unchanged file")
	}
}

func TestPromptCache_MissingFile(t *testing.T) {
	cache := &PromptCache{entries: map[string]*Prompt{}}
	_, err := cache.Load("/nonexistent/prompt.md")
	if err == nil {
		t.Fatal("expected error for missing prompt file")
	}
}

func TestSessionChatPrompt_FallsBackToBuiltin(t *testing.T) {
	// Override path to a missing file.
	t.Setenv("SESSION_CHAT_PROMPT_PATH", "/definitely/not/here.md")
	body, version := SessionChatPrompt()
	if !strings.Contains(body, "expert in adaptive video streaming") {
		t.Errorf("fallback body wrong: %q", body)
	}
	if version != "builtin-fallback" {
		t.Errorf("version = %q, want builtin-fallback", version)
	}
}

func TestSessionChatPrompt_LoadsFromDisk(t *testing.T) {
	src := `---
prompt_version: test-v1
---
# Custom prompt
test body
`
	path := writePromptTemp(t, src)
	t.Setenv("SESSION_CHAT_PROMPT_PATH", path)
	// Reset cache so previous tests don't shadow this one.
	defaultPromptCache = &PromptCache{entries: map[string]*Prompt{}}
	body, version := SessionChatPrompt()
	if !strings.Contains(body, "Custom prompt") || !strings.Contains(body, "test body") {
		t.Errorf("body wrong: %q", body)
	}
	if version != "test-v1" {
		t.Errorf("version = %q, want test-v1", version)
	}
}

func TestSessionChatPrompt_UnversionedFrontmatter(t *testing.T) {
	src := `---
default_max_tokens: 1000
---
body
`
	path := writePromptTemp(t, src)
	t.Setenv("SESSION_CHAT_PROMPT_PATH", path)
	defaultPromptCache = &PromptCache{entries: map[string]*Prompt{}}
	_, version := SessionChatPrompt()
	if version != "unversioned" {
		t.Errorf("version = %q, want unversioned", version)
	}
}
