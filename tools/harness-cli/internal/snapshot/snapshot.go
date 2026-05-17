// Package snapshot persists pre-mutation PlayerRecord state so the
// CLI can offer a meaningful `harness undo`. Every harness facade
// mutation is supposed to snapshot first per the project convention
// in feedback_harness_snapshot_before_mutate — without this, "undo
// that" can only reset to defaults, which is rarely what an
// operator actually wants.
//
// Storage path (per memory):
//
//	~/.claude/state/harness/<repo>/<player_id>-<unix-ms>.json
//
// Files are newest-first by sortable filename so `List` and `Latest`
// don't need to read each file to order them. The repo segment lets
// multiple checkouts share `~/.claude/state` without bleeding.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Snapshot is the on-disk record of one mutation. `Before` is the
// full PlayerRecord JSON immediately before the mutation; `Patch` is
// the merge-patch body actually sent. Either is opaque JSON because
// snapshot.go shouldn't depend on the v2 generated types — the
// facade decodes them when replaying.
type Snapshot struct {
	V          int             `json:"v"`
	Ts         time.Time       `json:"ts"`
	PlayerID   string          `json:"player_id"`
	Action     string          `json:"action"`
	EtagBefore string          `json:"etag_before,omitempty"`
	EtagAfter  string          `json:"etag_after,omitempty"`
	Before     json.RawMessage `json:"before,omitempty"`
	Patch      json.RawMessage `json:"patch,omitempty"`
}

// Store is the on-disk snapshot directory rooted at a per-repo path.
// Construct one via Open(repo) and reuse for the process lifetime.
type Store struct {
	Dir string
}

// Open returns the Store for a repo name. Repo should be the
// basename of the working tree (so multiple worktrees of the same
// upstream repo all see each other's snapshots — usually desired).
// Creates the directory on first use.
func Open(repo string) (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("snapshot: home dir: %w", err)
	}
	dir := filepath.Join(home, ".claude", "state", "harness", repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot: mkdir %s: %w", dir, err)
	}
	return &Store{Dir: dir}, nil
}

// Save writes the snapshot to disk. The filename is
// `<player_id>-<unix-ms>.json` so List/Latest can sort lexically.
// Errors are returned; the caller decides whether a snapshot failure
// should also fail the mutation (currently it does, by design — a
// silent snapshot miss would lie about undo coverage).
func (s *Store) Save(snap Snapshot) (string, error) {
	if snap.Ts.IsZero() {
		snap.Ts = time.Now().UTC()
	}
	if snap.V == 0 {
		snap.V = 1
	}
	name := fmt.Sprintf("%s-%013d.json", snap.PlayerID, snap.Ts.UnixMilli())
	path := filepath.Join(s.Dir, name)
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("snapshot: create %s: %w", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		return "", fmt.Errorf("snapshot: encode %s: %w", path, err)
	}
	return path, nil
}

// List returns snapshots newest-first, optionally filtered to a
// single player_id. `limit` caps the result; 0 returns all.
func (s *Store) List(playerID string, limit int) ([]Snapshot, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("snapshot: read dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if playerID != "" && !strings.HasPrefix(e.Name(), playerID+"-") {
			continue
		}
		names = append(names, e.Name())
	}
	// Newest first — filenames are `<uuid>-<unix-ms>.json` so a
	// reverse lexical sort works without parsing.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}
	out := make([]Snapshot, 0, len(names))
	for _, n := range names {
		snap, err := s.read(n)
		if err != nil {
			// Skip malformed files — one corrupt snapshot shouldn't
			// hide all the others. Surface via stderr at higher
			// levels if it matters.
			continue
		}
		out = append(out, snap)
	}
	return out, nil
}

// Latest returns the newest snapshot for a player, or os.ErrNotExist
// if none exists. Used by `harness undo` when no explicit snapshot
// id is given.
func (s *Store) Latest(playerID string) (Snapshot, string, error) {
	all, err := s.List(playerID, 1)
	if err != nil {
		return Snapshot{}, "", err
	}
	if len(all) == 0 {
		return Snapshot{}, "", os.ErrNotExist
	}
	name := fmt.Sprintf("%s-%013d.json", all[0].PlayerID, all[0].Ts.UnixMilli())
	return all[0], filepath.Join(s.Dir, name), nil
}

// FindByPrefix locates a single snapshot by filename prefix
// (typically the 8-char id prefix the operator copied from
// `snapshot list`). Returns ambiguity errors verbatim so the CLI
// can render them as-is.
func (s *Store) FindByPrefix(prefix string) (Snapshot, string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return Snapshot{}, "", err
	}
	var hits []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), prefix) {
			hits = append(hits, e.Name())
		}
	}
	switch len(hits) {
	case 0:
		return Snapshot{}, "", fmt.Errorf("no snapshot matches %q", prefix)
	case 1:
		snap, err := s.read(hits[0])
		return snap, filepath.Join(s.Dir, hits[0]), err
	default:
		return Snapshot{}, "", fmt.Errorf("snapshot prefix %q matched %d files", prefix, len(hits))
	}
}

func (s *Store) read(name string) (Snapshot, error) {
	path := filepath.Join(s.Dir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: parse %s: %w", path, err)
	}
	return snap, nil
}
