package har

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tests target the _extensions.context block introduced for issue #281.
// Lives in its own file so it can land independently of other test files
// (#278 har_test.go, #279 har_headers_test.go).

func TestBuild_ContextDeviceSurface(t *testing.T) {
	doc := Build(nil, BuildOptions{
		Context: &Context{
			Device: &DeviceContext{
				Model:       "iPad Pro 12.9 (6th gen)",
				OSVersion:   "iOS 18.2",
				AppVersion:  "1.4.2-abc123",
				NetworkType: "wifi",
			},
		},
	})
	ctx, ok := doc.Log.Extensions["context"].(*Context)
	if !ok {
		t.Fatalf("expected _extensions.context, got %T", doc.Log.Extensions["context"])
	}
	if ctx.Device.Model != "iPad Pro 12.9 (6th gen)" || ctx.Device.NetworkType != "wifi" {
		t.Errorf("device block wrong: %+v", ctx.Device)
	}
}

func TestBuild_ContextStreamSurface(t *testing.T) {
	doc := Build(nil, BuildOptions{
		Context: &Context{
			Stream: &StreamContext{
				ContentID:         "big-buck-bunny",
				Protocol:          "hls",
				Codec:             "h264",
				InitialVariantURL: "https://example.test/1080p.m3u8",
			},
		},
	})
	ctx := doc.Log.Extensions["context"].(*Context)
	if ctx.Stream.ContentID != "big-buck-bunny" || ctx.Stream.Protocol != "hls" {
		t.Errorf("stream block wrong: %+v", ctx.Stream)
	}
}

func TestBuild_ContextScenarioSurface(t *testing.T) {
	doc := Build(nil, BuildOptions{
		Context: &Context{
			Scenario: &ScenarioContext{
				FaultSettings: map[string]interface{}{
					"segment_failure_type":      "404",
					"segment_consecutive_failures": 3,
				},
				NftablesShape: map[string]interface{}{
					"nftables_bandwidth_mbps": 5.0,
					"nftables_delay_ms":       100,
				},
			},
		},
	})
	ctx := doc.Log.Extensions["context"].(*Context)
	if ctx.Scenario.FaultSettings["segment_failure_type"] != "404" {
		t.Errorf("scenario.fault_settings wrong: %+v", ctx.Scenario.FaultSettings)
	}
	if ctx.Scenario.NftablesShape["nftables_bandwidth_mbps"] != 5.0 {
		t.Errorf("scenario.nftables_shape wrong: %+v", ctx.Scenario.NftablesShape)
	}
}

func TestBuild_ContextTimingSurface(t *testing.T) {
	doc := Build(nil, BuildOptions{
		Context: &Context{
			Timing: &TimingContext{
				PlayStartedAt:   "2026-04-29T12:00:00Z",
				IncidentOffsetS: 47.3,
			},
		},
	})
	ctx := doc.Log.Extensions["context"].(*Context)
	if ctx.Timing.PlayStartedAt != "2026-04-29T12:00:00Z" {
		t.Errorf("timing.play_started_at wrong: %+v", ctx.Timing)
	}
	if ctx.Timing.IncidentOffsetS != 47.3 {
		t.Errorf("timing.incident_offset_s = %v, want 47.3", ctx.Timing.IncidentOffsetS)
	}
}

func TestBuild_ContextRecoveryChain(t *testing.T) {
	doc := Build(nil, BuildOptions{
		Context: &Context{
			RecoveryChain: []string{"frozen", "user_retry", "segment_stall"},
		},
	})
	ctx := doc.Log.Extensions["context"].(*Context)
	if len(ctx.RecoveryChain) != 3 || ctx.RecoveryChain[1] != "user_retry" {
		t.Errorf("recovery_chain wrong: %+v", ctx.RecoveryChain)
	}
}

func TestBuild_EmptyContextOmitted(t *testing.T) {
	// An entirely empty Context should NOT produce an
	// `_extensions.context: {}` block — it'd just be noise.
	doc := Build(nil, BuildOptions{
		Context: &Context{},
	})
	if _, has := doc.Log.Extensions["context"]; has {
		t.Errorf("empty context should not be emitted: %+v", doc.Log.Extensions)
	}
}

func TestBuild_NilContextOmitted(t *testing.T) {
	doc := Build(nil, BuildOptions{Context: nil})
	if _, has := doc.Log.Extensions["context"]; has {
		t.Errorf("nil context should not be emitted: %+v", doc.Log.Extensions)
	}
}

func TestBuild_ContextRoundTripsThroughJSON(t *testing.T) {
	doc := Build(nil, BuildOptions{
		SessionID: "s",
		Context: &Context{
			Device: &DeviceContext{Model: "Apple TV 4K", OSVersion: "tvOS 18"},
			Stream: &StreamContext{ContentID: "demo", Protocol: "hls"},
			Scenario: &ScenarioContext{
				FaultSettings: map[string]interface{}{"segment_failure_type": "timeout"},
			},
			RecoveryChain: []string{"frozen"},
		},
	})
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"recovery_chain":["frozen"]`) {
		t.Errorf("recovery_chain not in JSON: %s", body)
	}
	// Round-trip the whole thing and confirm the context survives.
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	logBlock := parsed["log"].(map[string]interface{})
	ext := logBlock["_extensions"].(map[string]interface{})
	ctx := ext["context"].(map[string]interface{})
	device := ctx["device"].(map[string]interface{})
	if device["model"] != "Apple TV 4K" {
		t.Errorf("device.model lost in round-trip: %+v", device)
	}
}
