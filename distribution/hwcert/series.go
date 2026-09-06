package hwcert

import (
	"fmt"
	"math"
	"sort"
)

// Series is one metric's raw timed samples, in the order they were taken.
type Series []float64

// Percentile is nearest-rank, so every figure it reports is a value that
// actually happened rather than an interpolation between two that did.
// Same definition #165 reports its baselines with, which keeps the two
// readable next to each other even though nothing compares them.
//
// An empty series has no percentile, and saying so is the point: a zero
// here travels into an evidence record as a measurement nobody took.
func (s Series) Percentile(p float64) (float64, bool) {
	if len(s) == 0 {
		return 0, false
	}
	sorted := append(Series(nil), s...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1], true
}

// Median is the 50th percentile, by the same nearest-rank rule.
func (s Series) Median() (float64, bool) { return s.Percentile(50) }

// ProcSample is one observation of one process. It records whether the
// process was there at all, and which process it was, rather than only how
// much memory and CPU it was using: without those two fields an app that
// is not running and an app that is idle produce identical numbers.
type ProcSample struct {
	AtSeconds float64 `json:"at_seconds"`
	Present   bool    `json:"present"`
	// PID and StartTicks together identify the process. /proc reuses
	// PIDs, so the start time (field 22 of /proc/<pid>/stat) is what
	// separates "the same process throughout" from "a new one each time",
	// which is what a crash loop looks like.
	PID        int     `json:"pid,omitempty"`
	StartTicks int64   `json:"start_ticks,omitempty"`
	RSSBytes   int64   `json:"rss_bytes,omitempty"`
	CPUSeconds float64 `json:"cpu_seconds,omitempty"`
}

// Liveness is what a window of samples turned out to be.
type Liveness string

const (
	// LivenessIdle is the app running, present throughout, under the
	// idle CPU limit it was classified against.
	LivenessIdle Liveness = "idle"
	// LivenessBusy is the app running and over that limit. Still a
	// measurement; whether it passes is the comparator's decision.
	LivenessBusy Liveness = "busy"
	// LivenessNotRunning is at least one sample with no process behind
	// it. Not an idle measurement, however little CPU it averages to.
	LivenessNotRunning Liveness = "not-running"
	// LivenessRestarted is a window spanning more than one process, which
	// is what a crash loop looks like from the outside.
	LivenessRestarted Liveness = "restarted"
	// LivenessTooFewSamples is a window with nothing to compute a rate
	// from.
	LivenessTooFewSamples Liveness = "too-few-samples"
	// LivenessSampled is a window that carries a measurement, without
	// saying whether it was a quiet one. Verify reports this: a window
	// sampled DURING a transfer is valid and is not supposed to be idle,
	// and calling it idle would be the report telling a small lie on
	// every run.
	LivenessSampled Liveness = "sampled"
)

// Measurable reports whether a window in this state carries a usable
// number. Only the first two do; the rest are refusals, and treating any
// of them as "0% CPU" is the mistake this type exists to prevent.
func (l Liveness) Measurable() bool {
	return l == LivenessIdle || l == LivenessBusy || l == LivenessSampled
}

// Window is one process sampled over time.
type Window struct {
	Process string       `json:"process"`
	Samples []ProcSample `json:"samples"`
}

// Validity answers whether this window carries a measurement at all,
// before anyone asks what the measurement is. It returns an empty Liveness
// when the window is usable.
func (w Window) Validity() (Liveness, string) {
	if len(w.Samples) < 2 {
		return LivenessTooFewSamples, fmt.Sprintf("%d sample(s); a rate needs at least two", len(w.Samples))
	}
	for i, s := range w.Samples {
		if !s.Present {
			return LivenessNotRunning, fmt.Sprintf("sample %d at %.0fs found no process; an absent process uses no CPU, which is not the same fact as an idle one", i, s.AtSeconds)
		}
	}
	first := w.Samples[0]
	for i, s := range w.Samples {
		if s.PID != first.PID || s.StartTicks != first.StartTicks {
			return LivenessRestarted, fmt.Sprintf("sample %d at %.0fs is pid %d started at %d, not pid %d started at %d; the window spans more than one process",
				i, s.AtSeconds, s.PID, s.StartTicks, first.PID, first.StartTicks)
		}
	}
	if w.Seconds() <= 0 {
		return LivenessTooFewSamples, "the window has no duration"
	}
	return "", ""
}

// Classify is Validity plus, for a usable window, whether it was idle or
// busy against the limit it is being held to.
func (w Window) Classify(idleCPUPercentLimit float64) (Liveness, string) {
	if l, why := w.Validity(); l != "" {
		return l, why
	}
	cpu, _ := w.CPUPercent()
	if cpu > idleCPUPercentLimit {
		return LivenessBusy, fmt.Sprintf("%.3f%% of one core over %.0fs, above the %.3f%% this window was classified against", cpu, w.Seconds(), idleCPUPercentLimit)
	}
	return LivenessIdle, fmt.Sprintf("%.3f%% of one core over %.0fs", cpu, w.Seconds())
}

// Seconds is the wall-clock span the samples cover.
func (w Window) Seconds() float64 {
	if len(w.Samples) < 2 {
		return 0
	}
	return w.Samples[len(w.Samples)-1].AtSeconds - w.Samples[0].AtSeconds
}

// CPUPercent is CPU-seconds consumed across the window, as a percentage of
// one core. Computed from the first and last cumulative readings, so a
// sampling interval that drifted does not change the answer.
func (w Window) CPUPercent() (float64, bool) {
	if len(w.Samples) < 2 {
		return 0, false
	}
	span := w.Seconds()
	if span <= 0 {
		return 0, false
	}
	used := w.Samples[len(w.Samples)-1].CPUSeconds - w.Samples[0].CPUSeconds
	return 100 * used / span, true
}

// PeakRSSBytes is the largest resident set seen. The peak rather than the
// mean: the question the budget answers is whether the app fits on the
// NAS, and it has to fit at its largest.
func (w Window) PeakRSSBytes() (int64, bool) {
	if len(w.Samples) == 0 {
		return 0, false
	}
	var peak int64
	var any bool
	for _, s := range w.Samples {
		if !s.Present {
			continue
		}
		any = true
		if s.RSSBytes > peak {
			peak = s.RSSBytes
		}
	}
	return peak, any
}
