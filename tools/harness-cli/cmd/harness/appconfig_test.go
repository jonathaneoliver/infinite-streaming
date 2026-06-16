package main

import "testing"

func TestBuildAppConfigBody(t *testing.T) {
	tests := []struct {
		name    string
		segment string
		proto   string
		offset  float64
		peak    int
		set     map[string]bool
		clear   bool
		want    string
		wantErr bool
	}{
		{
			name:  "clear wins",
			set:   map[string]bool{"segment": true},
			clear: true,
			want:  `{"app_config": null}`,
		},
		{
			name:    "segment only",
			segment: "s2",
			set:     map[string]bool{"segment": true},
			want:    `{"app_config":{"segment":"s2"}}`,
		},
		{
			name:    "all fields (keys sort alpha)",
			segment: "ll", proto: "dash", offset: 12, peak: 4,
			set:  map[string]bool{"segment": true, "protocol": true, "live-offset": true, "peak-bitrate": true},
			want: `{"app_config":{"live_offset_s":12,"peak_bitrate_mbps":4,"protocol":"dash","segment":"ll"}}`,
		},
		{
			name:   "explicit zeros are written when set",
			offset: 0, peak: 0,
			set:  map[string]bool{"live-offset": true, "peak-bitrate": true},
			want: `{"app_config":{"live_offset_s":0,"peak_bitrate_mbps":0}}`,
		},
		{
			name:    "bad segment errors",
			segment: "nope", set: map[string]bool{"segment": true}, wantErr: true,
		},
		{
			name:  "bad protocol errors",
			proto: "quic", set: map[string]bool{"protocol": true}, wantErr: true,
		},
		{
			name:   "negative offset errors",
			offset: -1, set: map[string]bool{"live-offset": true}, wantErr: true,
		},
		{
			name: "negative peak errors",
			peak: -1, set: map[string]bool{"peak-bitrate": true}, wantErr: true,
		},
		{
			name: "nothing set errors", set: map[string]bool{}, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildAppConfigBody(tt.segment, tt.proto, tt.offset, tt.peak, tt.set, tt.clear)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got body %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("body = %s, want %s", got, tt.want)
			}
		})
	}
}
