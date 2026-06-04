// Package aberration_crawl runs the invariants catalogue
// (invariants.yaml) against the analytics ClickHouse archive, grouped
// by the #550 Phase 4 version taxonomy. Issue #607.
package aberration_crawl

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Catalogue mirrors invariants.yaml.
type Catalogue struct {
	Version  int      `yaml:"version"`
	Defaults Defaults `yaml:"defaults"`
	Groups   Groups   `yaml:"groups"`
	Rules    []Rule   `yaml:"rules"`
}

type Defaults struct {
	Mode      string   `yaml:"mode"`
	Partition []string `yaml:"partition"`
	Order     string   `yaml:"order"`
}

type Groups struct {
	By                []string `yaml:"by"`
	UnversionedBucket bool     `yaml:"unversioned_bucket"`
}

type Rule struct {
	ID       string   `yaml:"id"`
	Tables   []string `yaml:"tables"`
	Kind     string   `yaml:"kind"` // row | vocab | monotonic | sequence | play | sql
	Severity string   `yaml:"severity"`
	Mode     string   `yaml:"mode"`    // census (default) | assert
	Pending  bool     `yaml:"pending"` // producing change not deployed; never assert
	Where    string   `yaml:"where"`   // optional row pre-filter

	// kind: row | sequence | play
	Violation string `yaml:"violation"`

	// kind: vocab
	Field   string   `yaml:"field"`
	Allowed []string `yaml:"allowed"`

	// kind: monotonic
	Fields []string `yaml:"fields"`

	// kind: sequence — lag_cols get prev_<col> aliases; columns lists
	// extra columns the violation references (so the inner SELECT can
	// stay narrow instead of SELECT *).
	LagCols []string `yaml:"lag_cols"`
	Columns []string `yaml:"columns"`

	// kind: play
	Aggregates map[string]string `yaml:"aggregates"`

	// kind: sql — sketch only; the runner logs and skips.
	SQLSketch string `yaml:"sql_sketch"`

	ApplicableSince ApplicableSince `yaml:"applicable_since"`
	Exclusions      []Exclusion     `yaml:"exclusions"`
	Source          string          `yaml:"source"`
	Rationale       string          `yaml:"rationale"`
}

type ApplicableSince struct {
	Date string `yaml:"date"`
	Note string `yaml:"note"`
}

type Exclusion struct {
	Name  string `yaml:"name"`
	Where string `yaml:"where"` // empty = documentation-only
	Note  string `yaml:"note"`
}

// SinceDate parses applicable_since.date; pending / unparseable dates
// fall back to the archive floor (census still runs, assert never does).
func (r Rule) SinceDate(archiveFloor string) string {
	if _, err := time.Parse("2006-01-02", r.ApplicableSince.Date); err != nil {
		return archiveFloor
	}
	return r.ApplicableSince.Date
}

func (r Rule) EffectiveMode(def string) string {
	m := r.Mode
	if m == "" {
		m = def
	}
	if r.Pending {
		return "census"
	}
	return m
}

func LoadCatalogue(path string) (*Catalogue, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalogue
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Catalogue) validate() error {
	seen := map[string]bool{}
	for _, r := range c.Rules {
		if r.ID == "" {
			return fmt.Errorf("rule with empty id")
		}
		if seen[r.ID] {
			return fmt.Errorf("duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
		if len(r.Tables) == 0 {
			return fmt.Errorf("rule %s: no tables", r.ID)
		}
		switch r.Kind {
		case "row", "sequence", "play":
			if r.Violation == "" {
				return fmt.Errorf("rule %s (kind %s): violation required", r.ID, r.Kind)
			}
			if r.Kind == "play" && len(r.Aggregates) == 0 {
				return fmt.Errorf("rule %s: play kind needs aggregates", r.ID)
			}
			if r.Kind == "sequence" && len(r.LagCols) == 0 {
				return fmt.Errorf("rule %s: sequence kind needs lag_cols", r.ID)
			}
		case "vocab":
			if r.Field == "" || len(r.Allowed) == 0 {
				return fmt.Errorf("rule %s: vocab kind needs field + allowed", r.ID)
			}
		case "monotonic":
			if len(r.Fields) == 0 {
				return fmt.Errorf("rule %s: monotonic kind needs fields", r.ID)
			}
		case "sql":
			// sketch-only; nothing to validate yet
		default:
			return fmt.Errorf("rule %s: unknown kind %q", r.ID, r.Kind)
		}
	}
	return nil
}
