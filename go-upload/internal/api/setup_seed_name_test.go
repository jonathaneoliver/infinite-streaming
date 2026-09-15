package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/util"
)

// Encode output named by outputNameForPartial (the first-run seed and the
// single-step POST /api/upload) must list with a codec, like chunked uploads.
// The ladder script names output `<output_name>_<codec>[_<tag>]`, and
// util.ListContent (which /api/content serves) only recognises the codec
// after a `_p<partial ms>_` marker. The iOS app's stream picker filters on
// that codec, so the old bare "sample_clip" name left it empty.
func TestSeedOutputListsWithCodec(t *testing.T) {
	if got := outputNameForPartial("sample_clip", defaultPartialDurationMs); got != "sample_clip_p200" {
		t.Fatalf("outputNameForPartial = %q, want sample_clip_p200 (same shape as chunked uploads)", got)
	}

	dir := t.TempDir()
	dirs := map[string]string{ // directory -> expected codec
		outputNameForPartial("sample_clip", defaultPartialDurationMs) + "_h264_xs": "h264",
		outputNameForPartial("sample_clip", defaultPartialDurationMs) + "_hevc_xs": "hevc",
		"sample_clip_h264_xs": "", // pre-fix seed name: no codec
	}
	for name := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "master.m3u8"), []byte("#EXTM3U\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	items, err := util.ListContent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(dirs) {
		t.Fatalf("ListContent returned %d items, want %d", len(items), len(dirs))
	}
	for _, it := range items {
		if want := dirs[it.Name]; it.Codec != want {
			t.Errorf("%s: codec = %q, want %q", it.Name, it.Codec, want)
		}
	}
}
