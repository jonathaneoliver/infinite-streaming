package charmatrix

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExampleSpecsExpand guards the shipped example matrices under
// tests/characterization/matrix/: every *.yaml must Load + Expand cleanly, so a
// grammar change can't silently break the runnable examples.
func TestExampleSpecsExpand(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "tests", "characterization", "matrix")
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Skipf("no example specs found under %s", dir)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			spec, err := Load(data)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			arms, err := Expand(spec)
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			if len(arms) == 0 {
				t.Fatalf("%s expanded to zero arms", filepath.Base(f))
			}
		})
	}
}
