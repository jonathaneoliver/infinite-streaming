package main

// llm_profiles.go — loader + types for the AI chat backend's
// profile catalog (#497).
//
// The catalog is read-only data the forwarder serves to the
// dashboard via GET /api/v2/chat/profiles. Users pick a template +
// model in the chat settings UI and supply their own api_key
// (browser localStorage). The forwarder never stores keys; the
// catalog never has secrets.
//
// `config/llm_profiles.yaml` ships embedded in the binary so the
// chat backend works out of the box. Set FORWARDER_LLM_PROFILES_PATH
// to override at runtime with an operator-edited file.

import (
	_ "embed"
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed config/llm_profiles.yaml
var embeddedProfileCatalog []byte

// LLMPricing is per-1M-tokens USD. A zero value means "free"
// (Ollama); a negative value (set by lookup miss) means "unknown".
type LLMPricing struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"  json:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok" json:"output_per_mtok"`
}

// LLMModel is one catalogue entry under a template.
type LLMModel struct {
	ID      string     `yaml:"id"      json:"id"`
	Label   string     `yaml:"label"   json:"label"`
	Pricing LLMPricing `yaml:"pricing" json:"pricing"`
	// ContextWindow is the model's maximum context size in tokens.
	// Surfaced in the chat panel's footer so the operator can see
	// how close they're getting to the model's hard limit before
	// the next send fails mid-stream. Zero = unknown (UI hides
	// the denominator).
	ContextWindow int `yaml:"context_window,omitempty" json:"context_window,omitempty"`
}

// LLMTemplate is one profile (Anthropic / HF / Ollama / …).
// Users select a template + model in the dashboard.
type LLMTemplate struct {
	Name           string     `yaml:"name"             json:"name"`
	Label          string     `yaml:"label"            json:"label"`
	BaseURL        string     `yaml:"base_url"         json:"base_url"`
	RequiresAPIKey bool       `yaml:"requires_api_key" json:"requires_api_key"`
	APIKeyHelp     string     `yaml:"api_key_help"     json:"api_key_help"`
	SupportsTools  bool       `yaml:"supports_tools"   json:"supports_tools"`
	Models         []LLMModel `yaml:"models"           json:"models"`
}

// LLMCatalog is the top-level YAML root.
type LLMCatalog struct {
	Templates []LLMTemplate `yaml:"templates" json:"templates"`
}

// catalogStore caches the parsed catalog. The file is small (<5KB)
// and changes only on operator edit; we reload on each request when
// the env var FORWARDER_LLM_PROFILES_RELOAD=1 is set, otherwise
// once at startup.
type catalogStore struct {
	mu    sync.RWMutex
	value *LLMCatalog
	path  string
}

var llmCatalog = &catalogStore{}

// LoadLLMCatalog reads + parses the YAML and caches the result.
// Path = "" loads the embedded default catalog (shipped with the
// binary). Non-empty path overrides with a file on disk. Safe to
// call concurrently; subsequent reads via LLMCatalogValue() return
// the cached value.
func LoadLLMCatalog(path string) (*LLMCatalog, error) {
	var body []byte
	source := "embedded default"
	if path == "" {
		body = embeddedProfileCatalog
	} else {
		var err error
		body, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("llm profiles: read %s: %w", path, err)
		}
		source = path
	}
	var c LLMCatalog
	if err := yaml.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("llm profiles: parse %s: %w", source, err)
	}
	if len(c.Templates) == 0 {
		return nil, fmt.Errorf("llm profiles: %s has zero templates", source)
	}
	// Per-template base_url override via env. Lets a deploy point
	// the "Local Ollama" entry at e.g. a remote Mac on the LAN
	// (`http://my-mac.local:11434/v1`) without touching the embedded
	// catalog. Pattern: FORWARDER_LLM_BASE_URL_<TEMPLATE_NAME>,
	// uppercased + hyphens → underscores. Examples:
	//   FORWARDER_LLM_BASE_URL_OLLAMA=http://my-mac.local:11434/v1
	//   FORWARDER_LLM_BASE_URL_CHATGPT_VIA_LITELLM=http://litellm:4000/v1
	applyEnvBaseURLOverrides(&c)
	llmCatalog.mu.Lock()
	llmCatalog.value = &c
	llmCatalog.path = source
	llmCatalog.mu.Unlock()
	return &c, nil
}

// applyEnvBaseURLOverrides walks every template and, if the env var
// FORWARDER_LLM_BASE_URL_<NAME> is set, replaces that template's
// base_url. Logs each override for operator visibility.
func applyEnvBaseURLOverrides(c *LLMCatalog) {
	for i := range c.Templates {
		t := &c.Templates[i]
		envName := "FORWARDER_LLM_BASE_URL_" + envSafeName(t.Name)
		if v := os.Getenv(envName); v != "" {
			old := t.BaseURL
			t.BaseURL = v
			fmt.Printf("llm profiles: %s base_url overridden by %s: %s → %s\n",
				t.Name, envName, old, v)
		}
	}
}

// envSafeName uppercases and replaces hyphens with underscores so
// "chatgpt-via-litellm" → "CHATGPT_VIA_LITELLM" for env-var lookup.
func envSafeName(name string) string {
	b := make([]byte, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 32 // upper
		case c == '-':
			b[i] = '_'
		default:
			b[i] = c
		}
	}
	return string(b)
}

// LLMCatalogValue returns the cached catalog. Returns nil before
// the first LoadLLMCatalog call.
func LLMCatalogValue() *LLMCatalog {
	llmCatalog.mu.RLock()
	defer llmCatalog.mu.RUnlock()
	return llmCatalog.value
}

// FindTemplate looks up a template by name. Returns nil when not
// found.
func (c *LLMCatalog) FindTemplate(name string) *LLMTemplate {
	for i := range c.Templates {
		if c.Templates[i].Name == name {
			return &c.Templates[i]
		}
	}
	return nil
}

// FindModel returns the catalog entry for (template, modelID), or
// nil if the model isn't listed (user typed a custom model id).
func (c *LLMCatalog) FindModel(templateName, modelID string) *LLMModel {
	t := c.FindTemplate(templateName)
	if t == nil {
		return nil
	}
	for i := range t.Models {
		if t.Models[i].ID == modelID {
			return &t.Models[i]
		}
	}
	return nil
}

// CostUSD computes the cost of a request. Returns -1 (unknown) when
// the model isn't in the catalog — the budget guard treats unknown
// as zero so the user isn't blocked, but the ledger surfaces the
// gap.
func (c *LLMCatalog) CostUSD(templateName, modelID string, inputTokens, outputTokens uint32) float64 {
	m := c.FindModel(templateName, modelID)
	if m == nil {
		return -1
	}
	const mTok = 1_000_000.0
	return (float64(inputTokens)/mTok)*m.Pricing.InputPerMTok +
		(float64(outputTokens)/mTok)*m.Pricing.OutputPerMTok
}
