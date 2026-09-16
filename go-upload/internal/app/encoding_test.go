package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkdirs creates each name as a directory under root, stamping mtimes in
// ascending order so "most recently modified" is deterministic rather than
// dependent on filesystem timestamp granularity.
func mkdirs(t *testing.T, root string, names ...string) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	for i, name := range names {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
}

func TestFindOutputDirectories(t *testing.T) {
	tests := []struct {
		name   string
		onDisk []string
		cfg    map[string]interface{}
		want   []string
	}{
		{
			// The pre-tag layout must keep working — existing content is not
			// being renamed.
			name:   "untagged legacy layout",
			onDisk: []string{"clip_p200_hevc", "clip_p200_h264"},
			cfg:    map[string]interface{}{"output_name": "clip_p200"},
			want:   []string{"clip_p200_hevc", "clip_p200_h264"},
		},
		{
			// The apple-uniq-live-xs default. Concatenating "_h264" missed
			// these entirely, which is the silent failure this guards.
			name:   "xs profile tag",
			onDisk: []string{"clip_p200_hevc_xs", "clip_p200_h264_xs"},
			cfg:    map[string]interface{}{"output_name": "clip_p200"},
			want:   []string{"clip_p200_hevc_xs", "clip_p200_h264_xs"},
		},
		{
			// Collision disambiguator appended by the script.
			name:   "tag plus timestamp",
			onDisk: []string{"clip_p200_h264_xs_20260101_010101"},
			cfg:    map[string]interface{}{"output_name": "clip_p200", "codec_selection": "h264"},
			want:   []string{"clip_p200_h264_xs_20260101_010101"},
		},
		{
			// A re-encode collided; the newest directory is the one this run
			// just wrote.
			name:   "newest wins on collision",
			onDisk: []string{"clip_p200_h264_xs", "clip_p200_h264_xs_20260101_010101"},
			cfg:    map[string]interface{}{"output_name": "clip_p200", "codec_selection": "h264"},
			want:   []string{"clip_p200_h264_xs_20260101_010101"},
		},
		{
			// The MPEG-TS package is a separate artifact and was never
			// reported; a tag must not sneak it in.
			name:   "ts package excluded",
			onDisk: []string{"clip_p200_h264_xs", "clip_p200_h264_ts_xs", "clip_p200_h264_ts"},
			cfg:    map[string]interface{}{"output_name": "clip_p200", "codec_selection": "h264"},
			want:   []string{"clip_p200_h264_xs"},
		},
		{
			// "all" previously matched none of the branches and returned an
			// empty list.
			name:   "codec all returns every codec",
			onDisk: []string{"clip_p200_hevc_xs", "clip_p200_h264_xs", "clip_p200_av1_xs"},
			cfg:    map[string]interface{}{"output_name": "clip_p200", "codec_selection": "all"},
			want:   []string{"clip_p200_hevc_xs", "clip_p200_h264_xs", "clip_p200_av1_xs"},
		},
		{
			name:   "missing codec is omitted",
			onDisk: []string{"clip_p200_h264_xs"},
			cfg:    map[string]interface{}{"output_name": "clip_p200"},
			want:   []string{"clip_p200_h264_xs"},
		},
		{
			// A different clip whose name merely starts the same must not be
			// picked up.
			name:   "prefix of another clip is not matched",
			onDisk: []string{"clip_p200_h264_xs", "clip_p200_h264extra_xs"},
			cfg:    map[string]interface{}{"output_name": "clip_p200", "codec_selection": "h264"},
			want:   []string{"clip_p200_h264_xs"},
		},
		{
			name:   "nothing produced",
			onDisk: []string{},
			cfg:    map[string]interface{}{"output_name": "clip_p200", "codec_selection": "h264"},
			want:   []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mkdirs(t, dir, tc.onDisk...)

			got := findOutputDirectories(tc.cfg, dir)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A plain file must never be reported as a package directory.
func TestFindOutputDirectoriesIgnoresFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "clip_p200_h264_xs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]interface{}{"output_name": "clip_p200", "codec_selection": "h264"}
	if got := findOutputDirectories(cfg, dir); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}
