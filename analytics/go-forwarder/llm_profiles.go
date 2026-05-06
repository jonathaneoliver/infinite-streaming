// LLM profile loader for the AI session-analysis path (epic #412).
//
// A profile pairs an OpenAI-compatible base URL + model + API-key env
// var. Profiles are loaded once at startup; live reload is intentionally
// out of scope (restart the forwarder to pick up new profiles).

package main

import (
	"fmt"
	"log"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

// Pricing is per-million-tokens USD; consumed by the llm_calls ledger
// (#417) to compute cost_usd. Zero means free (local models) and is
// honored by the daily-budget guard.
type Pricing struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
}

type LLMProfile struct {
	Name            string  `yaml:"-"`
	BaseURL         string  `yaml:"base_url"`
	APIKeyEnv       string  `yaml:"api_key_env"`
	Model           string  `yaml:"model"`
	SupportsTools   bool    `yaml:"supports_tools"`
	SupportsCaching bool    `yaml:"supports_caching"`
	Pricing         Pricing `yaml:"pricing"`
}

// Available means the API key env var is set (or none required).
// We surface this via /analytics/api/llm_profiles in #416 so the UI
// can grey out profiles instead of letting requests fail with 401.
func (p *LLMProfile) Available() bool {
	if p.APIKeyEnv == "" {
		return true
	}
	return os.Getenv(p.APIKeyEnv) != ""
}

func (p *LLMProfile) APIKey() string {
	if p.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(p.APIKeyEnv)
}

type LLMProfiles struct {
	Active   string                 `yaml:"active"`
	Profiles map[string]*LLMProfile `yaml:"profiles"`
}

func LoadLLMProfiles(path string) (*LLMProfiles, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read llm profiles %s: %w", path, err)
	}
	var p LLMProfiles
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse llm profiles %s: %w", path, err)
	}
	if len(p.Profiles) == 0 {
		return nil, fmt.Errorf("llm profiles %s: no profiles defined", path)
	}
	for name, prof := range p.Profiles {
		prof.Name = name
		if prof.BaseURL == "" {
			return nil, fmt.Errorf("llm profile %q: base_url is required", name)
		}
		if prof.Model == "" {
			return nil, fmt.Errorf("llm profile %q: model is required", name)
		}
	}
	// Map iteration order would otherwise make the default Active vary
	// across restarts. Pick lexicographically-smallest as a stable fallback.
	if p.Active == "" {
		names := make([]string, 0, len(p.Profiles))
		for n := range p.Profiles {
			names = append(names, n)
		}
		p.Active = slices.Min(names)
	}
	if _, ok := p.Profiles[p.Active]; !ok {
		return nil, fmt.Errorf("llm profiles %s: active profile %q not in profiles", path, p.Active)
	}
	return &p, nil
}

// loadLLMProfilesAtStartup is non-fatal by design: a misconfigured
// profiles file or a missing config volume must not stop session
// snapshot archival. session_chat handlers (#416) report 503 when
// llmProfiles is nil so the UI can grey out the AI features cleanly.
func loadLLMProfilesAtStartup(path string) {
	p, err := LoadLLMProfiles(path)
	if err != nil {
		log.Printf("llm profiles unavailable (%v); AI features disabled", err)
		llmProfiles = nil
		return
	}
	llmProfiles = p
	var avail, total int
	var names []string
	for _, prof := range p.Profiles {
		total++
		if prof.Available() {
			avail++
		}
		names = append(names, prof.Name)
	}
	log.Printf("llm profiles loaded: %d total, %d available, active=%q (%v)",
		total, avail, p.Active, names)
}

// Resolve picks the named profile, falling back to Active when name
// is empty. Errors if the profile doesn't exist or its key env is
// missing — callers translate this to the appropriate HTTP status.
func (p *LLMProfiles) Resolve(name string) (*LLMProfile, error) {
	if name == "" {
		name = p.Active
	}
	prof, ok := p.Profiles[name]
	if !ok {
		return nil, fmt.Errorf("unknown llm profile %q", name)
	}
	if !prof.Available() {
		return nil, fmt.Errorf("llm profile %q unavailable: env %s not set", name, prof.APIKeyEnv)
	}
	return prof, nil
}
