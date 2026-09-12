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

// A prober that stops watching a target must stop reporting on it. Its last
// reading would otherwise stand forever, and probe_up frozen at 0 is what the
// shipped alert rules call an outage.
func TestDropLabelRemovesEverySeriesForATarget(t *testing.T) {
	r := New()
	for _, target := range []string{"keep", "drop"} {
		r.SetGauge("streampulse_probe_up", "h", 1, map[string]string{"target": target})
		r.SetGauge("streampulse_segment_count", "h", 300,
			map[string]string{"target": target, "variant": "v0"})
		r.IncCounter("streampulse_findings_total", "h",
			map[string]string{"target": target, "check": "x", "severity": "critical"})
	}

	if n := r.DropLabel("target", "drop"); n != 3 {
		t.Errorf("dropped %d series, want 3", n)
	}
	body := scrape(t, r)
	if strings.Contains(body, `target="drop"`) {
		t.Errorf("the removed target is still reported:\n%s", body)
	}
	// Counters and gauges alike, across every metric name it appeared under.
	for _, want := range []string{
		`streampulse_probe_up{target="keep"} 1`,
		`streampulse_segment_count{target="keep",variant="v0"} 300`,
		`streampulse_findings_total{check="x",severity="critical",target="keep"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dropping one target disturbed another, missing %q", want)
		}
	}
}

func TestDropLabelIsSafeWhenNothingMatches(t *testing.T) {
	r := New()
	r.SetGauge("m", "h", 1, map[string]string{"target": "a"})
	if n := r.DropLabel("target", "nobody"); n != 0 {
		t.Errorf("dropped %d series for a target that was never there", n)
	}
	if !strings.Contains(scrape(t, r), `m{target="a"} 1`) {
		t.Error("an unmatched drop removed something")
	}
}

// The metric name stays registered, because the next target will use it.
func TestDroppedNameCanBeUsedAgain(t *testing.T) {
	r := New()
	r.SetGauge("streampulse_probe_up", "help text", 1, map[string]string{"target": "a"})
	r.DropLabel("target", "a")
	r.SetGauge("streampulse_probe_up", "help text", 1, map[string]string{"target": "b"})

	body := scrape(t, r)
	if !strings.Contains(body, "# HELP streampulse_probe_up help text") {
		t.Errorf("the help text was lost with the series:\n%s", body)
	}
	if !strings.Contains(body, `streampulse_probe_up{target="b"} 1`) {
		t.Errorf("the name could not be reused:\n%s", body)
	}
}
