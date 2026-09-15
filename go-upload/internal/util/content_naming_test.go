package util

import (
	"os"
	"path/filepath"
	"testing"
)

// #1032: infinite-streaming-encoder names padded encodes
// `<stem>_p200_padblack_<codec>[_<tag>]`. The codec must still be found, and
// the padding must stay in clip_id so padded and unpadded encodes of one
// source never dedup against each other.
func TestSplitClipIDAndCodec(t *testing.T) {
	cases := []struct {
		name, clipID, codec string
	}{
		// Unpadded — unchanged behaviour.
		{"redbull_p200_h264", "redbull", "h264"},
		{"redbull_p200_hevc", "redbull", "hevc"},
		{"fpv5_p200_h264_xs", "fpv5_xs", "h264"},
		{"fpv5_p200_hevc_6s", "fpv5_6s", "hevc"},
		{"INSANE_Clip_p200_H265_20260423_212139", "insane_clip", "h265"},
		{"clip_p200_av1_xs_20260101_010101", "clip_xs", "av1"},

		// Padded (#1032).
		{"clip_p200_padblack_h264", "clip_padblack", "h264"},
		{"clip_p200_padpink_hevc", "clip_padpink", "hevc"},
		{"clip_p200_padblack_h264_xs", "clip_padblack_xs", "h264"},
		{"clip_p200_PadBlack_AV1_6s_20260101_010101", "clip_padblack_6s", "av1"},

		// Still outside the contract: no codec.
		{"sample_clip_h264_xs", "sample_clip_h264_xs", ""},
		{"clip_p100_h264", "clip_p100_h264", ""},
		{"clip_p200_padgreen_h264", "clip_p200_padgreen_h264", ""},
		{"clip_p200_vp9", "clip_p200_vp9", ""},
	}
	for _, c := range cases {
		clipID, codec, _ := splitClipIDAndCodec(c.name)
		if clipID != c.clipID || codec != c.codec {
			t.Errorf("splitClipIDAndCodec(%q) = (%q, %q), want (%q, %q)", c.name, clipID, codec, c.clipID, c.codec)
		}
	}
}

// writeContent creates a minimal HLS package: a master naming the given
// variant URIs, and for each a media playlist with the given body.
func writeContent(t *testing.T, root, name string, variants map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	master := "#EXTM3U\n#EXT-X-VERSION:7\n"
	for uri, body := range variants {
		master += "#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=1280x720\n" + uri + "\n"
		p := filepath.Join(dir, filepath.FromSlash(uri))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(master), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const playlistWithParts = `#EXTM3U
#EXT-X-VERSION:10
#EXT-X-TARGETDURATION:7
#EXT-X-PART-INF:PART-TARGET=1.0
#EXT-X-MAP:URI="init.mp4"
#EXT-X-PART:DURATION=0.200200,URI="segment_00001.m4s",BYTERANGE="157047@432",INDEPENDENT=YES
#EXTINF:6.006000,
segment_00001.m4s
#EXTINF:6.006000,
segment_00002.m4s
`

const playlistNoParts = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:7
#EXT-X-MAP:URI="init.mp4"
#EXTINF:6.000000,
segment_00001.m4s
#EXTINF:6.000000,
segment_00002.m4s
`

// #1033c: variant playlists are found through the master, so a ladder that
// doesn't use the NNNp/playlist.m3u8 convention still reports LL, 1s and its
// native segment length.
func TestCatalogueFollowsMasterVariants(t *testing.T) {
	dir := writeContent(t, t.TempDir(), "byo_p200_h264", map[string]string{
		"video/hd.m3u8": playlistWithParts,
	})
	if !contentHasPartials(dir) {
		t.Error("contentHasPartials = false for #EXT-X-PART content outside NNNp/ dirs")
	}
	if d := detectSegmentDuration(dir); d == nil || *d != 6 {
		t.Errorf("detectSegmentDuration = %v, want 6", d)
	}
	got := availableSegmentDurations("byo_p200_h264", dir, true, detectSegmentDuration(dir))
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 6 {
		t.Errorf("availableSegmentDurations = %v, want [1 2 6]", got)
	}
}

// Content with no master still uses the legacy directory convention.
func TestCatalogueLegacyDirFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "720p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "720p", "playlist.m3u8"), []byte(playlistWithParts), 0o644); err != nil {
		t.Fatal(err)
	}
	if !contentHasPartials(dir) {
		t.Error("legacy 720p/playlist.m3u8 with parts not detected")
	}
}

// `#EXT-X-PART-INF` is a declaration, not partial info.
func TestPartInfAloneIsNotPartials(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-PART-INF:PART-TARGET=1.0\n#EXTINF:6.0,\nsegment_00001.m4s\n"
	dir := writeContent(t, t.TempDir(), "c_p200_h264", map[string]string{"720p/playlist.m3u8": body})
	if contentHasPartials(dir) {
		t.Error("#EXT-X-PART-INF alone counted as partials")
	}
}

// Sidecars count only when go-live would load one: `<segment>.byteranges`
// next to the playlist's segments — not any stray *.byteranges file.
func TestByterangesSidecarMustMatchSegment(t *testing.T) {
	root := t.TempDir()

	matching := writeContent(t, root, "a_p200_h264", map[string]string{"720p/playlist.m3u8": playlistNoParts})
	if contentHasPartials(matching) {
		t.Fatal("no parts and no sidecar should not report partials")
	}
	if err := os.WriteFile(filepath.Join(matching, "720p", "segment_00001.m4s.byteranges"), []byte(`{"fragments":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !contentHasPartials(matching) {
		t.Error("segment_00001.m4s.byteranges next to the playlist not detected")
	}

	stray := writeContent(t, root, "b_p200_h264", map[string]string{"720p/playlist.m3u8": playlistNoParts})
	if err := os.WriteFile(filepath.Join(stray, "720p", "other.byteranges"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if contentHasPartials(stray) {
		t.Error("a stray *.byteranges file counted as partials")
	}
}

// End to end through ListContent: a padded encode lists with its codec and
// does not hide (or get hidden by) its unpadded sibling.
func TestListContentPaddedAndUnpaddedSiblings(t *testing.T) {
	root := t.TempDir()
	writeContent(t, root, "clip_p200_h264_xs", map[string]string{"720p/playlist.m3u8": playlistWithParts})
	writeContent(t, root, "clip_p200_padblack_h264_xs", map[string]string{"720p/playlist.m3u8": playlistWithParts})

	list, err := ListContent(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListContent returned %d items, want 2 (padded and unpadded must not dedup): %+v", len(list), list)
	}
	byName := map[string]ContentInfo{}
	for _, c := range list {
		byName[c.Name] = c
	}
	padded := byName["clip_p200_padblack_h264_xs"]
	if padded.Codec != "h264" || padded.ClipID != "clip_padblack_xs" {
		t.Errorf("padded item: codec=%q clip_id=%q, want h264 / clip_padblack_xs", padded.Codec, padded.ClipID)
	}
	if !padded.HasLL {
		t.Error("padded item: has_ll = false")
	}
}
