package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// Two probers watching the same channel produce the same series names. Without
// something to tell them apart the second silently overwrites the first.
func TestConstantLabelsReachEverySeries(t *testing.T) {
	r := New()
	r.SetConstantLabels(map[string]string{"vantage": "eu-west"})
	r.SetGauge("streampulse_probe_up", "h", 1, map[string]string{"target": "ch1"})
	r.IncCounter("streampulse_findings_total", "h", map[string]string{"target": "ch1", "check": "x"})

	body := scrape(t, r)
	for _, want := range []string{
		`streampulse_probe_up{target="ch1",vantage="eu-west"} 1`,
		`vantage="eu-west"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q in:\n%s", want, body)
		}
	}
	if strings.Count(body, `vantage="eu-west"`) != 2 {
		t.Errorf("the label should be on every series:\n%s", body)
	}
}

// A caller must not be able to shadow it by accident.
func TestConstantLabelsWin(t *testing.T) {
	r := New()
	r.SetConstantLabels(map[string]string{"vantage": "eu-west"})
	r.SetGauge("m", "h", 1, map[string]string{"vantage": "wrong"})
	if body := scrape(t, r); !strings.Contains(body, `vantage="eu-west"`) {
		t.Errorf("a caller overrode the constant label:\n%s", body)
	}
}

// The default is no labels and no change, so a single-prober setup keeps the
// series identities its dashboards were built on.
func TestNoConstantLabelsChangesNothing(t *testing.T) {
	r := New()
	r.SetGauge("streampulse_probe_up", "h", 1, map[string]string{"target": "ch1"})
	body := scrape(t, r)
	if !strings.Contains(body, `streampulse_probe_up{target="ch1"} 1`) {
		t.Errorf("series identity changed:\n%s", body)
	}
	if strings.Contains(body, "vantage") {
		t.Errorf("an unset vantage leaked into the output:\n%s", body)
	}
}

// Snapshot feeds the web UI, and has to agree with the exposition format.
func TestSnapshotCarriesConstantLabels(t *testing.T) {
	r := New()
	r.SetConstantLabels(map[string]string{"vantage": "us-east"})
	r.SetGauge("m", "h", 2, map[string]string{"target": "ch1"})
	s := r.Snapshot()
	if len(s) != 1 || s[0].Labels["vantage"] != "us-east" || s[0].Labels["target"] != "ch1" {
		t.Errorf("snapshot = %+v", s)
	}
}
