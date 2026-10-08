package ollama

import (
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// TestParseSessionUsageLayout covers the settings page's current wording: the
// 5-hour window is labelled "Session usage" and reset times sit in <time>.
// The earlier parser only knew "rolling"/"5 hour", so it never found this
// window at all.
func TestParseSessionUsageLayout(t *testing.T) {
	now := time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC)
	page := `<html><body>
<h2>Cloud usage</h2><p>Pro</p>
<div><span>Session usage</span><span>42% used</span>
  <span>Resets <time datetime="2026-10-08T09:00:00Z">in 3 hours</time></span></div>
<div><span>Weekly usage</span><span>12.5% used</span>
  <span>Resets <local-time data-time="2026-10-12T00:00:00Z">Oct 12</local-time></span></div>
</body></html>`
	got := parse(page, now)
	if len(got.windows) != 2 {
		t.Fatalf("windows = %d, want 2: %+v", len(got.windows), got.windows)
	}
	want := map[string]struct {
		used  float64
		reset time.Time
	}{
		"5h": {42, time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)},
		"7d": {12.5, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)},
	}
	for _, w := range got.windows {
		exp, ok := want[w.Name]
		if !ok {
			t.Fatalf("unexpected window %q", w.Name)
		}
		if w.UsedPercent == nil || *w.UsedPercent != exp.used {
			t.Errorf("%s used = %v, want %v", w.Name, w.UsedPercent, exp.used)
		}
		if w.ResetAt == nil || !w.ResetAt.Equal(exp.reset) {
			t.Errorf("%s reset = %v, want %v", w.Name, w.ResetAt, exp.reset)
		}
		if w.Scope != domain.ScopeAccount || w.Label == "" {
			t.Errorf("%s must be an account window with a label: %+v", w.Name, w)
		}
	}
}

// TestParseRemainingIsConvertedToUsed: a page that shows what is left must
// not be read as usage.
func TestParseRemainingIsConvertedToUsed(t *testing.T) {
	page := `<div><p>Session usage</p><p>70% remaining</p></div>`
	got := parse(page, time.Now())
	if len(got.windows) != 1 || got.windows[0].UsedPercent == nil || *got.windows[0].UsedPercent != 30 {
		t.Fatalf("remaining not converted to used: %+v", got.windows)
	}
}
