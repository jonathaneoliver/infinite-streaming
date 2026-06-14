package sweep

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrAlreadyClaimed is returned by Claim when another runner won the race
// (the atomic rename found no backlog file). The caller should try the next
// candidate, not retry this one.
var ErrAlreadyClaimed = errors.New("sweep: experiment already claimed")

// ErrNotFound is returned when an experiment file is absent in the bucket.
var ErrNotFound = errors.New("sweep: experiment not found")

// Store is the on-disk `.sweep/` queue. All worktree runners share one Store
// on one filesystem; the atomic rename in Claim is the only synchronisation.
type Store struct {
	Root string
}

// DefaultRoot is `.sweep` in the current directory, overridable via SWEEP_ROOT.
func DefaultRoot() string {
	if r := os.Getenv("SWEEP_ROOT"); r != "" {
		return r
	}
	return ".sweep"
}

// Open returns a Store rooted at root, creating the status subdirectories if
// they don't exist. Safe to call repeatedly.
func Open(root string) (*Store, error) {
	s := &Store{Root: root}
	for _, st := range AllStatuses {
		if err := os.MkdirAll(s.dir(st), 0o755); err != nil {
			return nil, fmt.Errorf("sweep: create %s dir: %w", st, err)
		}
	}
	return s, nil
}

func (s *Store) dir(st Status) string             { return filepath.Join(s.Root, string(st)) }
func (s *Store) path(st Status, id string) string { return filepath.Join(s.dir(st), id+".json") }

// Save writes e into the given status bucket atomically (temp file + rename),
// overwriting any existing file for that id in that bucket.
func (s *Store) Save(st Status, e *Experiment) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("sweep: marshal %s: %w", e.ID, err)
	}
	b = append(b, '\n')
	final := s.path(st, e.ID)
	tmp := filepath.Join(s.dir(st), "."+e.ID+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("sweep: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("sweep: commit %s: %w", final, err)
	}
	return nil
}

// Load reads one experiment from a bucket.
func (s *Store) Load(st Status, id string) (*Experiment, error) {
	b, err := os.ReadFile(s.path(st, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var e Experiment
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("sweep: parse %s/%s: %w", st, id, err)
	}
	return &e, nil
}

// List returns every experiment in a bucket, sorted by id for determinism.
// Dotfiles (in-flight temp writes) are skipped.
func (s *Store) List(st Status) ([]*Experiment, error) {
	entries, err := os.ReadDir(s.dir(st))
	if err != nil {
		return nil, err
	}
	var out []*Experiment
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		e, err := s.Load(st, id)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Counts returns the number of experiments in each bucket.
func (s *Store) Counts() (map[Status]int, error) {
	counts := make(map[Status]int, len(AllStatuses))
	for _, st := range AllStatuses {
		es, err := s.List(st)
		if err != nil {
			return nil, err
		}
		counts[st] = len(es)
	}
	return counts, nil
}

// Claim atomically moves an experiment from backlog → running and stamps the
// owner + claim time. The os.Rename is the lock: if two runners race for the
// same id, only one rename succeeds; the loser gets ErrAlreadyClaimed and
// should move on. `now` (RFC3339 UTC) is recorded as ClaimedAt so the
// stale-claim reaper can recover files orphaned by a dead runner (§11); pass
// "" to skip stamping (e.g. in tests that don't exercise the reaper).
func (s *Store) Claim(id, owner, now string) (*Experiment, error) {
	src := s.path(StatusBacklog, id)
	dst := s.path(StatusRunning, id)
	if err := os.Rename(src, dst); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrAlreadyClaimed
		}
		return nil, err
	}
	// We now exclusively own the running/ file.
	e, err := s.Load(StatusRunning, id)
	if err != nil {
		return nil, err
	}
	e.Owner = owner
	e.ClaimedAt = now
	if err := s.Save(StatusRunning, e); err != nil {
		return nil, err
	}
	return e, nil
}

// Move writes e into the `to` bucket and removes its file from the `from`
// bucket — the post-verdict transition (running → done/found/review/feedback).
// Unlike Claim this is not a race point: only the owning runner touches a
// running/ file. The write-then-remove order means a crash mid-Move leaves the
// experiment in both buckets (recoverable) rather than vanishing.
func (s *Store) Move(from, to Status, e *Experiment) error {
	if err := s.Save(to, e); err != nil {
		return err
	}
	if err := os.Remove(s.path(from, e.ID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sweep: remove %s/%s: %w", from, e.ID, err)
	}
	return nil
}
