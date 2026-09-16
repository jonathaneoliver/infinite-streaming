package plays

import "testing"

func TestEnrichScenario_HybridSources(t *testing.T) {
	rows := []map[string]any{{
		"device_class":     "phone",
		"device_model":     "iPhone15,2",
		"player_tech":      "AVPlayer",
		"app_version":      "1.4.0",
		"content_id":       "tears-of-steel",
		"os_version_major": float64(18), // JSON number, as ClickHouse UInt decodes
		"os_version_minor": float64(1),
		"label_histogram": []any{
			[]any{"testing=test_rampup", float64(1)},
			[]any{"testing=platform_ipad-sim", float64(1)},
			[]any{"testing=run_id_20260524T070148Z", float64(1)},
			[]any{"critical=qoe_stall", float64(3)}, // non-testing label ignored
		},
	}}
	enrichScenario(rows)

	sc, ok := rows[0]["scenario"].(map[string]any)
	if !ok {
		t.Fatalf("scenario not attached: %#v", rows[0]["scenario"])
	}
	want := map[string]string{
		"device_class": "phone",
		"device_model": "iPhone15,2",
		"player_tech":  "AVPlayer",
		"app_version":  "1.4.0",
		"content_id":   "tears-of-steel",
		"os_version":   "18.1",
		"test":         "rampup",
		"platform":     "ipad-sim",
		"run_id":       "20260524T070148Z",
	}
	for k, v := range want {
		if got, _ := sc[k].(string); got != v {
			t.Errorf("scenario[%q] = %q, want %q", k, got, v)
		}
	}
	if _, leaked := sc["qoe_stall"]; leaked {
		t.Errorf("non-testing label leaked into scenario: %#v", sc)
	}
}

func TestEnrichScenario_OmittedWhenEmpty(t *testing.T) {
	rows := []map[string]any{{
		"play_id":         "abc",
		"label_histogram": []any{[]any{"info=play_start", float64(1)}},
	}}
	enrichScenario(rows)
	if _, present := rows[0]["scenario"]; present {
		t.Errorf("scenario should be absent for a play with no identity fields, got %#v", rows[0]["scenario"])
	}
}

func TestEnrichScenario_OSMajorOnly(t *testing.T) {
	rows := []map[string]any{{"os_version_major": float64(17)}}
	enrichScenario(rows)
	sc := rows[0]["scenario"].(map[string]any)
	if got, _ := sc["os_version"].(string); got != "17" {
		t.Errorf("os_version = %q, want %q (no spurious .minor / .0)", got, "17")
	}
}

func TestEnrichScenario_DeviceFromClassOnly(t *testing.T) {
	// A play with device_class but no model still surfaces the class.
	rows := []map[string]any{{"device_class": "tv"}}
	enrichScenario(rows)
	sc := rows[0]["scenario"].(map[string]any)
	if got, _ := sc["device_class"].(string); got != "tv" {
		t.Errorf("device_class = %q, want tv", got)
	}
	if _, hasModel := sc["device_model"]; hasModel {
		t.Errorf("device_model should be absent: %#v", sc)
	}
}

func TestEnrichScenario_ServerSideFields(t *testing.T) {
	rows := []map[string]any{{
		"master_manifest_url": "http://h/go-live/bbb/2s/master.m3u8",
		"served_by":           "go-live/v2.0.0",
	}}
	enrichScenario(rows)
	sc := rows[0]["scenario"].(map[string]any)
	if got, _ := sc["manifest_variant"].(string); got != "2s" {
		t.Errorf("manifest_variant = %q, want 2s", got)
	}
	if got, _ := sc["server_build"].(string); got != "v2.0.0" {
		t.Errorf("server_build = %q, want v2.0.0", got)
	}
}

func TestManifestVariant(t *testing.T) {
	cases := map[string]string{
		"":                                       "",
		"http://h/go-live/c/master.m3u8":         "ll",
		"http://h/go-live/c/2s/master.m3u8":      "2s",
		"http://h/go-live/c/master_2s.m3u8":      "2s",
		"http://h/go-live/c/6s/master.m3u8":      "6s",
		"http://h/go-live/c/master_6s.m3u8":      "6s",
		"http://h/go-live/c/1080p/index.m3u8":    "ll",
	}
	for url, want := range cases {
		if got := manifestVariant(url); got != want {
			t.Errorf("manifestVariant(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestServerBuild(t *testing.T) {
	cases := map[string]string{
		"go-live/v2.0.0": "v2.0.0",
		"go-live/abc123": "abc123",
		"go-live":        "", // dev build, no ldflags — no build info
		"":               "",
		"nginx":          "",
	}
	for hdr, want := range cases {
		if got := serverBuild(hdr); got != want {
			t.Errorf("serverBuild(%q) = %q, want %q", hdr, got, want)
		}
	}
}
