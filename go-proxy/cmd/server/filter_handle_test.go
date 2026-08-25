package main

import "testing"

// Realistic `tc filter show dev eth0 parent 1:0` output: a u32 hashtable line
// (fh 800:) plus three leaf filters, one per session port, each a sport match
// at offset 20. Ports: 30181=0x75e5→1:181, 30381=0x76ad→1:381, 30481=0x7711→1:481.
const sampleFilterShow = `filter parent 1: protocol ip pref 1 u32 chain 0
filter parent 1: protocol ip pref 1 u32 chain 0 fh 800: ht divisor 1
filter parent 1: protocol ip pref 1 u32 chain 0 fh 800::800 order 2048 key ht 800 bkt 0 flowid 1:181 not_in_hw
  match 75e50000/ffff0000 at 20
filter parent 1: protocol ip pref 1 u32 chain 0 fh 800::801 order 2049 key ht 800 bkt 0 flowid 1:381 not_in_hw
  match 76ad0000/ffff0000 at 20
filter parent 1: protocol ip pref 1 u32 chain 0 fh 800::802 order 2050 key ht 800 bkt 0 flowid 1:481 not_in_hw
  match 77110000/ffff0000 at 20
`

func TestU32HandleForPort_resolvesExactHandle(t *testing.T) {
	cases := map[int]string{
		30181: "800::800", // 0x75e5
		30381: "800::801", // 0x76ad
		30481: "800::802", // 0x7711
	}
	for port, want := range cases {
		if got := u32HandleForPort(sampleFilterShow, port); got != want {
			t.Errorf("u32HandleForPort(port=%d) = %q, want %q", port, got, want)
		}
	}
}

func TestU32HandleForPort_absentPortReturnsEmpty(t *testing.T) {
	// 30581=0x7775 has no filter — must return "" so RemoveFilter no-ops rather
	// than falling back to a match-spec delete (the #816 over-deletion).
	if got := u32HandleForPort(sampleFilterShow, 30581); got != "" {
		t.Errorf("absent port: got %q, want empty", got)
	}
}

func TestU32HandleForPort_emptyInput(t *testing.T) {
	if got := u32HandleForPort("", 30181); got != "" {
		t.Errorf("empty input: got %q, want empty", got)
	}
}

func TestU32HandleForPort_dportFallback(t *testing.T) {
	// The dport fallback form: 0000<port>/0000ffff at offset 20. 30181=0x75e5.
	show := "filter parent 1: protocol ip pref 1 u32 chain 0 fh 800::805 order 2053 key ht 800 bkt 0 flowid 1:181\n  match 000075e5/0000ffff at 20\n"
	if got := u32HandleForPort(show, 30181); got != "800::805" {
		t.Errorf("dport fallback: got %q, want 800::805", got)
	}
}

func TestU32HandleForPort_skipsHashtableHandle(t *testing.T) {
	// A match must never bind to the `fh 800:` hashtable handle (no `::`).
	show := "filter parent 1: protocol ip pref 1 u32 chain 0 fh 800: ht divisor 1\n  match 75e50000/ffff0000 at 20\n"
	if got := u32HandleForPort(show, 30181); got != "" {
		t.Errorf("hashtable-only: got %q, want empty (no leaf handle)", got)
	}
}
