package main

import (
	"net/http"
	"net/url"
	"testing"
)

func TestCapturedHeaders_FiltersSensitive(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "test")
	h.Set("Range", "bytes=0-1023")
	h.Set("Cookie", "session=secret")
	h.Set("Authorization", "Bearer xyz")
	h.Set("Set-Cookie", "abc=def")
	h.Set("X-Amz-Security-Token", "stoken")
	h.Set("Content-Type", "video/mp4")

	out := capturedHeaders(h)
	names := map[string]bool{}
	for _, p := range out {
		names[p.Name] = true
	}
	for _, banned := range []string{"Cookie", "Authorization", "Set-Cookie", "X-Amz-Security-Token"} {
		if names[banned] {
			t.Errorf("sensitive header %q leaked into capture: %+v", banned, out)
		}
	}
	for _, expected := range []string{"User-Agent", "Range", "Content-Type"} {
		if !names[expected] {
			t.Errorf("expected header %q missing from capture: %+v", expected, out)
		}
	}
}

func TestCapturedHeaders_StableOrder(t *testing.T) {
	// Map iteration order is non-deterministic in Go; capturedHeaders
	// must sort so two runs of the same input produce identical HAR
	// diffs.
	h := http.Header{}
	h.Set("Z-Header", "z")
	h.Set("A-Header", "a")
	h.Set("M-Header", "m")

	a := capturedHeaders(h)
	b := capturedHeaders(h)
	if len(a) != len(b) {
		t.Fatalf("captures of same input have different lengths")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("position %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
	// Verify alphabetical.
	if a[0].Name != "A-Header" || a[1].Name != "M-Header" || a[2].Name != "Z-Header" {
		t.Errorf("not sorted: %+v", a)
	}
}

func TestCapturedHeaders_PreservesRepeatedValues(t *testing.T) {
	h := http.Header{}
	h.Add("Accept", "video/mp4")
	h.Add("Accept", "application/json")
	out := capturedHeaders(h)
	count := 0
	for _, p := range out {
		if p.Name == "Accept" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 Accept entries, got %d: %+v", count, out)
	}
}

func TestCapturedHeaders_EmptyInput(t *testing.T) {
	if got := capturedHeaders(http.Header{}); got != nil {
		t.Errorf("empty input should return nil, got %+v", got)
	}
	if got := capturedHeaders(nil); got != nil {
		t.Errorf("nil input should return nil, got %+v", got)
	}
}

func TestCapturedQueryString_PreservesURLOrder(t *testing.T) {
	u, err := url.Parse("https://example.test/playlist.m3u8?player_id=p1&play_id=42&codec=h264")
	if err != nil {
		t.Fatal(err)
	}
	out := capturedQueryString(u)
	if len(out) != 3 {
		t.Fatalf("expected 3 params, got %d", len(out))
	}
	want := []struct{ name, value string }{
		{"player_id", "p1"},
		{"play_id", "42"},
		{"codec", "h264"},
	}
	for i, w := range want {
		if out[i].Name != w.name || out[i].Value != w.value {
			t.Errorf("position %d: got %+v, want %s=%s", i, out[i], w.name, w.value)
		}
	}
}

func TestCapturedQueryString_DecodesPercent(t *testing.T) {
	u, _ := url.Parse("https://example.test/x?path=%2Fhome%2Fuser&q=hello%20world")
	out := capturedQueryString(u)
	if out[0].Name != "path" || out[0].Value != "/home/user" {
		t.Errorf("percent decode failed: %+v", out[0])
	}
	if out[1].Value != "hello world" {
		t.Errorf("space decode failed: %+v", out[1])
	}
}

func TestCapturedQueryString_NoQuery(t *testing.T) {
	u, _ := url.Parse("https://example.test/x")
	if got := capturedQueryString(u); got != nil {
		t.Errorf("URL without query should yield nil, got %+v", got)
	}
}

func TestCapturedQueryString_FlagOnlyParam(t *testing.T) {
	// Bare param like `?debug` should appear with empty value.
	u, _ := url.Parse("https://example.test/x?debug")
	out := capturedQueryString(u)
	if len(out) != 1 || out[0].Name != "debug" || out[0].Value != "" {
		t.Errorf("flag-only param wrong: %+v", out)
	}
}

func TestStampNetMeta_DoesntOverwriteExisting(t *testing.T) {
	pre := []HeaderPair{{Name: "pre", Value: "v"}}
	entry := NetworkLogEntry{
		RequestHeaders: pre,
	}
	stampNetMeta(&entry, []HeaderPair{{Name: "new", Value: "v"}}, nil, nil)
	if len(entry.RequestHeaders) != 1 || entry.RequestHeaders[0].Name != "pre" {
		t.Errorf("stamp overwrote pre-set headers: %+v", entry.RequestHeaders)
	}
}

func TestStampNetMeta_NilEntryIsSafe(t *testing.T) {
	// Defensive — no panic.
	stampNetMeta(nil, []HeaderPair{{Name: "x", Value: "y"}}, nil, nil)
}

func TestStampNetMeta_PopulatesFromCaptures(t *testing.T) {
	entry := NetworkLogEntry{}
	req := []HeaderPair{{Name: "User-Agent", Value: "t"}}
	qs := []HeaderPair{{Name: "p", Value: "v"}}
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"text/plain"}}}
	stampNetMeta(&entry, req, qs, resp)
	if len(entry.RequestHeaders) != 1 || entry.RequestHeaders[0].Name != "User-Agent" {
		t.Errorf("RequestHeaders not populated: %+v", entry.RequestHeaders)
	}
	if len(entry.QueryString) != 1 {
		t.Errorf("QueryString not populated: %+v", entry.QueryString)
	}
	if len(entry.ResponseHeaders) != 1 || entry.ResponseHeaders[0].Name != "Content-Type" {
		t.Errorf("ResponseHeaders not populated: %+v", entry.ResponseHeaders)
	}
}
