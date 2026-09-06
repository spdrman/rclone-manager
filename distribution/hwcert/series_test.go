package hwcert

import (
	"strings"
	"testing"
)

func TestPercentileIsNearestRankAndRefusesAnEmptySeries(t *testing.T) {
	s := Series{5, 1, 4, 2, 3}

	// Nearest-rank, so every answer is a value that actually happened.
	for _, tc := range []struct {
		p    float64
		want float64
	}{
		{50, 3},
		{95, 5},
		{100, 5},
		{1, 1},
	} {
		got, ok := s.Percentile(tc.p)
		if !ok {
			t.Fatalf("p%v: not ok", tc.p)
		}
		if got != tc.want {
			t.Errorf("p%v = %v, want %v", tc.p, got, tc.want)
		}
	}

	if got, ok := s.Median(); !ok || got != 3 {
		t.Errorf("median = %v (ok=%v), want 3", got, ok)
	}

	// An empty series has no p95, and saying so is the whole point: a
	// zero here would travel all the way into an evidence record as a
	// measurement nobody took.
	if v, ok := (Series{}).Percentile(95); ok {
		t.Errorf("an empty series reported p95 = %v", v)
	}
	if v, ok := (Series{}).Median(); ok {
		t.Errorf("an empty series reported a median = %v", v)
	}
}

// idleSamples builds a window of n samples interval seconds apart, in which
// the process is present throughout and burns cpuPerSample CPU-seconds
// between each pair.
func idleSamples(n int, interval, cpuPerSample float64) Window {
	w := Window{Process: "backup-manager-web serve"}
	for i := 0; i < n; i++ {
		w.Samples = append(w.Samples, ProcSample{
			AtSeconds:  float64(i) * interval,
			Present:    true,
			PID:        4242,
			StartTicks: 900100,
			RSSBytes:   90_000_000 + int64(i),
			CPUSeconds: float64(i) * cpuPerSample,
		})
	}
	return w
}

// The distinction this test exists for: a process consuming no CPU and a
// process that is not there consume exactly the same amount of CPU. Only
// one of them is an idle app, and the harness has to be able to tell them
// apart without being told which it is looking at.
func TestIdleIsDistinguishableFromNotRunning(t *testing.T) {
	const limit = 1.0

	t.Run("idle", func(t *testing.T) {
		// 121 samples, 5s apart, 0.002 CPU-seconds each: 0.24
		// CPU-seconds over 600s, which is 0.04% of one core.
		got, why := idleSamples(121, 5, 0.002).Classify(limit)
		if got != LivenessIdle {
			t.Fatalf("classified as %q (%s), want %q", got, why, LivenessIdle)
		}
		if !got.Measurable() {
			t.Error("an idle window should be measurable")
		}
	})

	t.Run("not running", func(t *testing.T) {
		w := idleSamples(121, 5, 0.002)
		// The process goes away two thirds of the way through. Every
		// later sample reports nothing, which averages out to less CPU
		// than the idle window above.
		for i := 80; i < len(w.Samples); i++ {
			w.Samples[i] = ProcSample{AtSeconds: w.Samples[i].AtSeconds}
		}
		got, why := w.Classify(limit)
		if got != LivenessNotRunning {
			t.Fatalf("classified as %q (%s), want %q", got, why, LivenessNotRunning)
		}
		if got.Measurable() {
			t.Error("a window in which the process vanished must not be measurable")
		}
		if !strings.Contains(why, "80") {
			t.Errorf("the reason %q does not say which sample lost the process", why)
		}
	})

	t.Run("never running at all", func(t *testing.T) {
		w := Window{Process: "backup-manager-web serve"}
		for i := 0; i < 121; i++ {
			w.Samples = append(w.Samples, ProcSample{AtSeconds: float64(i) * 5})
		}
		if got, _ := w.Classify(limit); got != LivenessNotRunning {
			t.Errorf("classified as %q, want %q", got, LivenessNotRunning)
		}
	})

	t.Run("crash looping", func(t *testing.T) {
		w := idleSamples(121, 5, 0.002)
		// Present in every sample, no CPU to speak of, and a different
		// process each time. Without the PID and the start time this is
		// indistinguishable from an idle app.
		for i := 60; i < len(w.Samples); i++ {
			w.Samples[i].PID = 5150
			w.Samples[i].StartTicks = 930400
			w.Samples[i].CPUSeconds = float64(i-60) * 0.002
		}
		got, why := w.Classify(limit)
		if got != LivenessRestarted {
			t.Fatalf("classified as %q (%s), want %q", got, why, LivenessRestarted)
		}
		if got.Measurable() {
			t.Error("a window spanning a restart must not be measurable")
		}
	})

	t.Run("busy", func(t *testing.T) {
		// 0.1 CPU-seconds per 5s sample is 2% of one core, over the 1%
		// limit. Busy is still a measurement, it is just one that fails
		// its threshold; that decision belongs to the comparator.
		got, _ := idleSamples(121, 5, 0.1).Classify(limit)
		if got != LivenessBusy {
			t.Fatalf("classified as %q, want %q", got, LivenessBusy)
		}
		if !got.Measurable() {
			t.Error("a busy window is still a measurement")
		}
	})

	t.Run("too few samples", func(t *testing.T) {
		if got, _ := idleSamples(1, 5, 0).Classify(limit); got != LivenessTooFewSamples {
			t.Errorf("classified as %q, want %q", got, LivenessTooFewSamples)
		}
		if got, _ := (Window{}).Classify(limit); got != LivenessTooFewSamples {
			t.Errorf("an empty window classified as %q, want %q", got, LivenessTooFewSamples)
		}
	})
}

func TestWindowAggregatesOnlyWhatItActuallySampled(t *testing.T) {
	w := idleSamples(121, 5, 0.002)

	if got := w.Seconds(); got != 600 {
		t.Errorf("Seconds = %v, want 600", got)
	}
	cpu, ok := w.CPUPercent()
	if !ok {
		t.Fatal("CPUPercent not ok on a full window")
	}
	if cpu < 0.039 || cpu > 0.041 {
		t.Errorf("CPUPercent = %v, want about 0.04", cpu)
	}
	rss, ok := w.PeakRSSBytes()
	if !ok || rss != 90_000_120 {
		t.Errorf("PeakRSSBytes = %v (ok=%v), want 90000120", rss, ok)
	}

	// Nothing sampled, nothing to report. Not zero.
	if v, ok := (Window{}).CPUPercent(); ok {
		t.Errorf("an empty window reported CPUPercent = %v", v)
	}
	if v, ok := (Window{}).PeakRSSBytes(); ok {
		t.Errorf("an empty window reported PeakRSSBytes = %v", v)
	}
}
