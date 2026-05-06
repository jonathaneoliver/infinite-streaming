package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTempYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "profiles.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write tmp yaml: %v", err)
	}
	return p
}

func TestLoadLLMProfiles_HappyPath(t *testing.T) {
	p := writeTempYAML(t, `
active: opus
profiles:
  opus:
    base_url: https://api.anthropic.com/v1/openai/
    api_key_env: ANTHROPIC_API_KEY
    model: claude-opus-4-7
    supports_tools: true
    supports_caching: true
    pricing:
      input_per_mtok: 15.0
      output_per_mtok: 75.0
  local:
    base_url: http://ollama:11434/v1
    api_key_env: ""
    model: llama3.1:70b
    supports_tools: true
    supports_caching: false
    pricing:
      input_per_mtok: 0
      output_per_mtok: 0
`)
	profiles, err := LoadLLMProfiles(p)
	if err != nil {
		t.Fatalf("LoadLLMProfiles: %v", err)
	}
	if got := profiles.Active; got != "opus" {
		t.Errorf("Active = %q, want %q", got, "opus")
	}
	if profiles.Profiles["opus"].Name != "opus" {
		t.Errorf("Name not populated from map key")
	}
	if !profiles.Profiles["local"].Available() {
		t.Errorf("local profile (no api_key_env) should be available")
	}
}

func TestLoadLLMProfiles_DefaultsActiveLexicographically(t *testing.T) {
	p := writeTempYAML(t, `
profiles:
  zebra:
    base_url: http://x/v1
    api_key_env: ""
    model: m1
  alpha:
    base_url: http://x/v1
    api_key_env: ""
    model: m2
  middle:
    base_url: http://x/v1
    api_key_env: ""
    model: m3
`)
	for i := 0; i < 5; i++ {
		profiles, err := LoadLLMProfiles(p)
		if err != nil {
			t.Fatalf("LoadLLMProfiles: %v", err)
		}
		if profiles.Active != "alpha" {
			t.Errorf("iter %d: Active = %q, want %q (lexicographic min)", i, profiles.Active, "alpha")
		}
	}
}

func TestLoadLLMProfiles_MissingFile(t *testing.T) {
	_, err := LoadLLMProfiles("/nonexistent/path.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadLLMProfiles_MalformedYAML(t *testing.T) {
	p := writeTempYAML(t, "this is not: yaml: {{{ broken")
	_, err := LoadLLMProfiles(p)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestLoadLLMProfiles_EmptyProfiles(t *testing.T) {
	p := writeTempYAML(t, "active: foo\nprofiles: {}\n")
	_, err := LoadLLMProfiles(p)
	if err == nil {
		t.Fatal("expected error for empty profiles map")
	}
}

func TestLoadLLMProfiles_MissingBaseURL(t *testing.T) {
	p := writeTempYAML(t, `
profiles:
  bad:
    api_key_env: FOO
    model: m
`)
	_, err := LoadLLMProfiles(p)
	if err == nil {
		t.Fatal("expected error for missing base_url")
	}
}

func TestLoadLLMProfiles_MissingModel(t *testing.T) {
	p := writeTempYAML(t, `
profiles:
  bad:
    base_url: http://x/v1
    api_key_env: FOO
`)
	_, err := LoadLLMProfiles(p)
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestLoadLLMProfiles_ActiveNotInProfiles(t *testing.T) {
	p := writeTempYAML(t, `
active: notreal
profiles:
  real:
    base_url: http://x/v1
    api_key_env: ""
    model: m
`)
	_, err := LoadLLMProfiles(p)
	if err == nil {
		t.Fatal("expected error when active points to missing profile")
	}
}

func TestProfile_AvailableTracksEnv(t *testing.T) {
	const envName = "LLM_TEST_KEY_DO_NOT_SET_IN_CI"
	os.Unsetenv(envName)
	t.Cleanup(func() { os.Unsetenv(envName) })

	p := writeTempYAML(t, `
profiles:
  paid:
    base_url: http://x/v1
    api_key_env: `+envName+`
    model: m
`)
	profiles, err := LoadLLMProfiles(p)
	if err != nil {
		t.Fatalf("LoadLLMProfiles: %v", err)
	}
	prof := profiles.Profiles["paid"]
	if prof.Available() {
		t.Errorf("expected unavailable when env unset")
	}
	t.Setenv(envName, "secret-value")
	if !prof.Available() {
		t.Errorf("expected available after env set")
	}
	if prof.APIKey() != "secret-value" {
		t.Errorf("APIKey() = %q, want %q", prof.APIKey(), "secret-value")
	}
}

func TestResolve_FallsBackToActive(t *testing.T) {
	p := writeTempYAML(t, `
active: free
profiles:
  free:
    base_url: http://x/v1
    api_key_env: ""
    model: m
`)
	profiles, err := LoadLLMProfiles(p)
	if err != nil {
		t.Fatalf("LoadLLMProfiles: %v", err)
	}
	prof, err := profiles.Resolve("")
	if err != nil {
		t.Fatalf("Resolve(\"\"): %v", err)
	}
	if prof.Name != "free" {
		t.Errorf("Resolve(\"\").Name = %q, want %q", prof.Name, "free")
	}
}

func TestResolve_UnknownProfile(t *testing.T) {
	p := writeTempYAML(t, `
profiles:
  free:
    base_url: http://x/v1
    api_key_env: ""
    model: m
`)
	profiles, _ := LoadLLMProfiles(p)
	_, err := profiles.Resolve("nope")
	if err == nil {
		t.Fatal("expected error for unknown profile")
	}
}

func TestResolve_UnavailableProfile(t *testing.T) {
	const envName = "LLM_TEST_KEY_RESOLVE_UNAVAIL"
	os.Unsetenv(envName)
	t.Cleanup(func() { os.Unsetenv(envName) })
	p := writeTempYAML(t, `
profiles:
  paid:
    base_url: http://x/v1
    api_key_env: `+envName+`
    model: m
`)
	profiles, _ := LoadLLMProfiles(p)
	_, err := profiles.Resolve("paid")
	if err == nil {
		t.Fatal("expected error when API key env missing")
	}
}
