package runner

import "testing"

func TestTunnelListHasUDID(t *testing.T) {
	const udid = "00008120-000242DE1152201E"
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty list", `[]`, false},
		{"present", `[{"address":"fd73::1","rsdPort":52146,"udid":"00008120-000242DE1152201E","userspaceTun":true}]`, true},
		{"case-insensitive", `[{"udid":"00008120-000242de1152201e"}]`, true},
		{"other device only", `[{"udid":"AAAAAAAA-1111-2222-3333-444444444444"}]`, false},
		{"malformed json", `not json`, false},
		{"multi has it", `[{"udid":"BBBB"},{"udid":"00008120-000242DE1152201E"}]`, true},
	}
	for _, c := range cases {
		if got := tunnelListHasUDID([]byte(c.raw), udid); got != c.want {
			t.Errorf("%s: tunnelListHasUDID=%v, want %v", c.name, got, c.want)
		}
	}
}
