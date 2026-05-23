package main

// llm_tools_tier2.go — context tools that ground the chat backend
// in project knowledge (#497). All read-only; all operate against
// the `.claude/` tree the project keeps as canonical knowledge
// (skills, standards, findings, conventions).
//
// Files stay on disk under cfg.claudeDir. The forwarder reads them
// per-request — they're small (<10KB each, dozens to low hundreds of
// files total) so caching is unnecessary at v1 scale and operator
// edits show up on the next call.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// safeName allows only [a-zA-Z0-9._/-] in path components — prevents
// any `../` escape from a malicious or confused LLM. Slashes are
// allowed only because skills live in subdirectories
// (`.claude/skills/finding/SKILL.md`); the directory traversal
// rejection happens via filepath.Clean comparison below.
var safeName = regexp.MustCompile(`^[a-zA-Z0-9._/-]{1,128}$`)

func validateSlug(name string) error {
	if name == "" {
		return fmt.Errorf("name required")
	}
	if !safeName.MatchString(name) {
		return fmt.Errorf("invalid name (allowed: a-z, A-Z, 0-9, ., _, -, max 128 chars)")
	}
	cleaned := filepath.Clean(name)
	if cleaned != name || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "/..") {
		return fmt.Errorf("invalid name (path traversal)")
	}
	return nil
}

// Tier2Tools builds the context tool set. claudeDir is the absolute
// path to the project's .claude/ directory (mounted into the
// container; FORWARDER_CLAUDE_DIR env var).
func Tier2Tools(claudeDir string) []Tool {
	if claudeDir == "" {
		// Empty dir = tools degrade to "no knowledge available"
		// instead of failing — the chat still works, just without
		// findings/standards/skills citations.
		claudeDir = "/dev/null/.claude"
	}
	return []Tool{
		listFindingsTool(claudeDir),
		readFindingTool(claudeDir),
		listStandardsTool(claudeDir),
		readStandardTool(claudeDir),
		listSkillsTool(claudeDir),
		readSkillTool(claudeDir),
		readConventionsTool(claudeDir),
	}
}

// --- Findings ---

func listFindingsTool(claudeDir string) Tool {
	return Tool{
		Name: "list_findings",
		Description: "List the project's recorded findings (discoveries from past " +
			"investigations). Each finding is a tagged hypothesis with " +
			"timeline + evidence. Use this BEFORE concluding anything novel " +
			"— the symptom you're looking at may already be in the library. " +
			"Use read_finding(slug) to pull the full text.",
		Tier: 2,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"grep": map[string]any{"type": "string", "description": "Optional substring filter against filename (e.g. 'stall', 'ipad', '2026-05')."},
			},
		},
		Execute: func(_ context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				Grep string `json:"grep"`
			}
			if len(args) > 0 {
				_ = json.Unmarshal(args, &a)
			}
			entries, err := os.ReadDir(filepath.Join(claudeDir, "findings"))
			if err != nil {
				return mustJSON(map[string]any{"count": 0, "findings": []string{}, "error": err.Error()}), nil
			}
			out := []string{}
			needle := strings.ToLower(a.Grep)
			for _, e := range entries {
				name := e.Name()
				if !strings.HasSuffix(name, ".md") {
					continue
				}
				slug := strings.TrimSuffix(name, ".md")
				if needle != "" && !strings.Contains(strings.ToLower(slug), needle) {
					continue
				}
				out = append(out, slug)
			}
			sort.Strings(out)
			return mustJSON(map[string]any{"count": len(out), "findings": out}), nil
		},
	}
}

func readFindingTool(claudeDir string) Tool {
	return Tool{
		Name: "read_finding",
		Description: "Read a finding's full markdown body. The slug is the filename " +
			"without .md (e.g. 'ipad-262s-stall-2026-05-17'). If a sibling " +
			".json sidecar exists with the raw player state at the moment of " +
			"capture, it's returned alongside.",
		Tier: 2,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"slug": map[string]any{"type": "string", "description": "Finding slug (filename without .md)."},
			},
			"required": []string{"slug"},
		},
		Execute: func(_ context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				Slug string `json:"slug"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if err := validateSlug(a.Slug); err != nil {
				return "", err
			}
			mdPath := filepath.Join(claudeDir, "findings", a.Slug+".md")
			md, err := os.ReadFile(mdPath)
			if err != nil {
				return mustJSON(map[string]any{"found": false, "slug": a.Slug, "error": err.Error()}), nil
			}
			result := map[string]any{
				"found":    true,
				"slug":     a.Slug,
				"markdown": string(md),
			}
			// Optional JSON sidecar.
			if sidecar, err := os.ReadFile(filepath.Join(claudeDir, "findings", a.Slug+".json")); err == nil {
				var parsed any
				if err := json.Unmarshal(sidecar, &parsed); err == nil {
					result["sidecar"] = parsed
				}
			}
			return mustJSON(result), nil
		},
	}
}

// --- Standards ---

func listStandardsTool(claudeDir string) Tool {
	return Tool{
		Name: "list_standards",
		Description: "List the project's domain standards documents (HLS taxonomy, " +
			"ABR decision model, AVPlayer quirks, codec strings, fault " +
			"injection wire contract, characterization principles, harness CLI, " +
			"startup/abort characterization tests). Use read_standard(name) for " +
			"the full text. Always ground reasoning about player behavior in " +
			"the relevant standard before guessing.",
		Tier: 2,
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Execute: func(_ context.Context, _ json.RawMessage, _ ToolEmitter) (string, error) {
			entries, err := os.ReadDir(filepath.Join(claudeDir, "standards"))
			if err != nil {
				return mustJSON(map[string]any{"count": 0, "standards": []string{}, "error": err.Error()}), nil
			}
			out := []string{}
			for _, e := range entries {
				name := e.Name()
				if !strings.HasSuffix(name, ".md") {
					continue
				}
				out = append(out, strings.TrimSuffix(name, ".md"))
			}
			sort.Strings(out)
			return mustJSON(map[string]any{"count": len(out), "standards": out}), nil
		},
	}
}

func readStandardTool(claudeDir string) Tool {
	return Tool{
		Name: "read_standard",
		Description: "Read a domain standard's full markdown body. The name is the " +
			"filename without .md (e.g. 'hls-taxonomy', 'abr-decision-model').",
		Tier: 2,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []string{"name"},
		},
		Execute: func(_ context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if err := validateSlug(a.Name); err != nil {
				return "", err
			}
			body, err := os.ReadFile(filepath.Join(claudeDir, "standards", a.Name+".md"))
			if err != nil {
				return mustJSON(map[string]any{"found": false, "name": a.Name, "error": err.Error()}), nil
			}
			return mustJSON(map[string]any{"found": true, "name": a.Name, "markdown": string(body)}), nil
		},
	}
}

// --- Skills ---

func listSkillsTool(claudeDir string) Tool {
	return Tool{
		Name: "list_skills",
		Description: "List the project's playbook skills (triage, investigate, " +
			"forensics, finding, fault, shape, harness). Each skill is a " +
			"procedure for a particular kind of analysis. Use read_skill(name) " +
			"to load a skill's playbook before following its procedure.",
		Tier: 2,
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Execute: func(_ context.Context, _ json.RawMessage, _ ToolEmitter) (string, error) {
			entries, err := os.ReadDir(filepath.Join(claudeDir, "skills"))
			if err != nil {
				return mustJSON(map[string]any{"count": 0, "skills": []string{}, "error": err.Error()}), nil
			}
			out := []string{}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				out = append(out, e.Name())
			}
			sort.Strings(out)
			return mustJSON(map[string]any{"count": len(out), "skills": out}), nil
		},
	}
}

func readSkillTool(claudeDir string) Tool {
	return Tool{
		Name: "read_skill",
		Description: "Read a skill's playbook (SKILL.md). Skills describe a " +
			"step-by-step procedure for a kind of analysis. Follow the " +
			"procedure's intent, substituting this chat's typed tools " +
			"(find_plays, get_control_events, etc.) for the harness CLI " +
			"commands the skill references — those are for Claude Code, not " +
			"for the web chat backend.",
		Tier: 2,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []string{"name"},
		},
		Execute: func(_ context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if err := validateSlug(a.Name); err != nil {
				return "", err
			}
			body, err := os.ReadFile(filepath.Join(claudeDir, "skills", a.Name, "SKILL.md"))
			if err != nil {
				return mustJSON(map[string]any{"found": false, "name": a.Name, "error": err.Error()}), nil
			}
			return mustJSON(map[string]any{"found": true, "name": a.Name, "markdown": string(body)}), nil
		},
	}
}

// --- Conventions ---

func readConventionsTool(claudeDir string) Tool {
	return Tool{
		Name: "read_conventions",
		Description: "Read the cross-skill conventions document. Covers project-wide " +
			"rules: tagging causal claims confirmed/refuted/needs-test, local " +
			"vs UTC time display, citation style, no-guessing-during-triage. " +
			"You should follow these conventions in every response.",
		Tier: 2,
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Execute: func(_ context.Context, _ json.RawMessage, _ ToolEmitter) (string, error) {
			body, err := os.ReadFile(filepath.Join(claudeDir, "skills", "CONVENTIONS.md"))
			if err != nil {
				return mustJSON(map[string]any{"found": false, "error": err.Error()}), nil
			}
			return mustJSON(map[string]any{"found": true, "markdown": string(body)}), nil
		},
	}
}
