package charmatrix

import (
	"strings"
	"testing"
)

func TestRenderTable_LandedColumn(t *testing.T) {
	armA := &Arm{ID: "m/a", Platform: "ipad-sim", Segment: "s6", ProxyLiveOffset: f64(24)}
	armB := &Arm{ID: "m/b", Platform: "ipad-sim", Segment: "s2", AppLiveOffset: f64(30)}
	armC := &Arm{ID: "m/c", Platform: "ipad-sim", Segment: "s6"} // not a live-offset arm

	results := []ArmResult{
		{Arm: armA, IntendedOff: 24, AchievedOff: 24, HasOffset: true, Landed: true, PlayID: "abcdef1234"},
		{Arm: armB, IntendedOff: 30, AchievedOff: 12, HasOffset: true, Landed: false, Note: "IV did not move"},
		{Arm: armC}, // no offset → landed must read "-"
	}
	out := RenderTable("m", results)

	if !strings.Contains(out, "matrix: m (3 arms)") {
		t.Errorf("missing header: %q", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// layout: [0]=title, [1]=header, [2]=underline, [3..]=data rows.
	if !strings.Contains(lines[3], "yes") {
		t.Errorf("arm A should be landed=yes: %q", lines[3])
	}
	if !strings.Contains(lines[4], "NO") {
		t.Errorf("arm B should be landed=NO: %q", lines[4])
	}
	// arm C: no intended offset, so src/intended/achieved/landed all "-".
	if strings.Contains(lines[5], "yes") || strings.Contains(lines[5], "NO") {
		t.Errorf("arm C (no offset) should not show a landed verdict: %q", lines[5])
	}
}

func TestRenderTable_DryRunNoFalseLanded(t *testing.T) {
	// A live-offset arm with no measurement (dry run) must show "-" for landed,
	// not a no-data default reading as pass/fail.
	arm := &Arm{ID: "m/a", Platform: "ipad-sim", ProxyLiveOffset: f64(24)}
	out := RenderTable("m", []ArmResult{{Arm: arm, IntendedOff: 24}})
	dataLine := strings.Split(strings.TrimRight(out, "\n"), "\n")[3]
	if strings.Contains(dataLine, "yes") || strings.Contains(dataLine, "NO") {
		t.Errorf("dry-run arm must not show a landed verdict: %q", dataLine)
	}
}

func TestRenderTable_ErrShownAsNote(t *testing.T) {
	arm := &Arm{ID: "m/a", Platform: "ipad-sim"}
	out := RenderTable("m", []ArmResult{{Arm: arm, Err: "bootstrap: proxy returned 503"}})
	if !strings.Contains(out, "ERR: bootstrap: proxy returned 503") {
		t.Errorf("error not surfaced in note column: %q", out)
	}
}
