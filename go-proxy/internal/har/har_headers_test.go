package har

import (
	"encoding/json"
	"testing"
)

// These tests target the request/response headers + queryString
// plumbing added in issue #279. They live in a separate file from
// har_test.go (PR #278) so the two PRs can land in any order.

func TestBuild_RequestHeadersFlowThrough(t *testing.T) {
	src := Source{
		URL:    "https://proxy.test/seg.m4s",
		Status: 200,
		RequestHeaders: []NameValue{
			{Name: "User-Agent", Value: "InfiniteStreamPlayer/1.0"},
			{Name: "Range", Value: "bytes=0-1023"},
			{Name: "If-None-Match", Value: `"abc123"`},
		},
	}
	doc := Build([]Source{src}, BuildOptions{})
	headers := doc.Log.Entries[0].Request.Headers
	if len(headers) != 3 {
		t.Fatalf("expected 3 request headers, got %d: %+v", len(headers), headers)
	}
	if headers[1].Name != "Range" || headers[1].Value != "bytes=0-1023" {
		t.Errorf("Range header mapping wrong: %+v", headers[1])
	}
}

func TestBuild_ResponseHeadersFlowThrough(t *testing.T) {
	src := Source{
		URL:    "https://proxy.test/seg.m4s",
		Status: 200,
		ResponseHeaders: []NameValue{
			{Name: "Content-Type", Value: "video/mp4"},
			{Name: "Cache-Control", Value: "no-store"},
			{Name: "Server", Value: "nginx"},
		},
	}
	doc := Build([]Source{src}, BuildOptions{})
	headers := doc.Log.Entries[0].Response.Headers
	if len(headers) != 3 {
		t.Fatalf("expected 3 response headers, got %d: %+v", len(headers), headers)
	}
}

func TestBuild_QueryStringFlowsThrough(t *testing.T) {
	src := Source{
		URL:    "https://proxy.test/playlist.m3u8?player_id=p1&play_id=42",
		Status: 200,
		QueryString: []NameValue{
			{Name: "player_id", Value: "p1"},
			{Name: "play_id", Value: "42"},
		},
	}
	doc := Build([]Source{src}, BuildOptions{})
	qs := doc.Log.Entries[0].Request.QueryString
	if len(qs) != 2 {
		t.Fatalf("expected 2 query params, got %d: %+v", len(qs), qs)
	}
	if qs[0].Name != "player_id" || qs[0].Value != "p1" {
		t.Errorf("first param wrong: %+v", qs[0])
	}
}

func TestBuild_EmptyMetadataYieldsEmptyArrays(t *testing.T) {
	// Per HAR 1.2 spec, request.headers/queryString and response.headers
	// must be present (empty array, not null) so a HAR consumer can
	// always iterate. Builder should produce []NameValue{} when the
	// source has none.
	doc := Build([]Source{{URL: "x", Status: 200}}, BuildOptions{})
	e := doc.Log.Entries[0]
	if e.Request.Headers == nil {
		t.Errorf("Request.Headers should be []NameValue{}, got nil")
	}
	if e.Request.QueryString == nil {
		t.Errorf("Request.QueryString should be []NameValue{}, got nil")
	}
	if e.Response.Headers == nil {
		t.Errorf("Response.Headers should be []NameValue{}, got nil")
	}
}

func TestBuild_HeadersSurviveJSONRoundTrip(t *testing.T) {
	// Real HAR consumers parse the JSON. Make sure our headers and
	// query params reach the wire shape DevTools/Charles expect.
	src := Source{
		URL:    "https://proxy.test/seg.m4s?player_id=p1",
		Status: 200,
		RequestHeaders: []NameValue{
			{Name: "User-Agent", Value: "test"},
		},
		ResponseHeaders: []NameValue{
			{Name: "Content-Type", Value: "video/mp4"},
		},
		QueryString: []NameValue{
			{Name: "player_id", Value: "p1"},
		},
	}
	doc := Build([]Source{src}, BuildOptions{})
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	logBlock := parsed["log"].(map[string]interface{})
	entries := logBlock["entries"].([]interface{})
	entry := entries[0].(map[string]interface{})
	request := entry["request"].(map[string]interface{})
	response := entry["response"].(map[string]interface{})

	reqHeaders := request["headers"].([]interface{})
	if len(reqHeaders) != 1 {
		t.Errorf("request.headers count = %d, want 1", len(reqHeaders))
	}
	respHeaders := response["headers"].([]interface{})
	if len(respHeaders) != 1 {
		t.Errorf("response.headers count = %d, want 1", len(respHeaders))
	}
	qs := request["queryString"].([]interface{})
	if len(qs) != 1 {
		t.Errorf("queryString count = %d, want 1", len(qs))
	}
	first := reqHeaders[0].(map[string]interface{})
	if first["name"] != "User-Agent" || first["value"] != "test" {
		t.Errorf("header round-trip lost data: %+v", first)
	}
}
