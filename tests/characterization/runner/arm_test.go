package runner

import (
	"reflect"
	"testing"
)

// ladder11 mirrors insane_new_p200_h264's 11-rung catalogue ladder, deliberately
// shuffled to prove ApplyKeep sorts by bandwidth before selecting.
func ladder11() []Variant {
	return []Variant{
		{Resolution: "1280x720", Height: 720, Bandwidth: 3552350},
		{Resolution: "640x360", Height: 360, Bandwidth: 1063012},
		{Resolution: "3840x2160", Height: 2160, Bandwidth: 30333359},
		{Resolution: "960x540", Height: 540, Bandwidth: 1893473},
		{Resolution: "2560x1440", Height: 1440, Bandwidth: 15484068},
		{Resolution: "768x432", Height: 432, Bandwidth: 1420320},
		{Resolution: "1920x1080", Height: 1080, Bandwidth: 7182067},
		{Resolution: "1152x648", Height: 648, Bandwidth: 2571743},
		{Resolution: "3200x1800", Height: 1800, Bandwidth: 21525717},
		{Resolution: "1600x900", Height: 900, Bandwidth: 5034445},
		{Resolution: "2304x1296", Height: 1296, Bandwidth: 10530479},
	}
}

func TestApplyKeepEveryOther(t *testing.T) {
	got := ApplyKeep("every_other", ladder11())
	// sorted asc, keep i%2==0 || i==n-1: 360,540,720,1080,1440,2160
	want := []string{"640x360", "960x540", "1280x720", "1920x1080", "2560x1440", "3840x2160"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("every_other keep-set = %v, want %v", got, want)
	}
}

func TestApplyKeepAllAndUnknownAreNoOp(t *testing.T) {
	for _, rule := range []string{"", "all", "bogus"} {
		if got := ApplyKeep(rule, ladder11()); got != nil {
			t.Errorf("ApplyKeep(%q) = %v, want nil (no thinning)", rule, got)
		}
	}
}

func TestContentAllowedVariantsConfig(t *testing.T) {
	if cfg := ContentAllowedVariantsConfig(nil); cfg != nil {
		t.Errorf("empty keep-set should yield nil cfg, got %v", cfg)
	}
	cfg := ContentAllowedVariantsConfig([]string{"640x360", "1280x720"})
	want := BootstrapConfig{
		"content.allowed_variants[0]": "640x360",
		"content.allowed_variants[1]": "1280x720",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("ContentAllowedVariantsConfig = %v, want %v", cfg, want)
	}
}
