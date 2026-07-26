package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Unit coverage for the sampling helpers behind the NFR ratio budgets in
// cmd_start_drift_bench_test.go. The budgets are only trustworthy if the
// statistics under them are, and both helpers are easy to get subtly wrong
// (percentile index convention, integer-division mean).

// TestSampleInterleavedAlternatesPairOrder proves the bias-canceling property
// the NFR ratios depend on: across a run, each callback opens the pair exactly
// half the time. If one always went first it would absorb every cold-start and
// scheduling penalty, inflating the ratio for reasons unrelated to the code.
func TestSampleInterleavedAlternatesPairOrder(t *testing.T) {
	const iterations = 10
	var order []string
	_, _, err := sampleInterleaved(iterations,
		func() error { order = append(order, "measured"); return nil },
		func() error { order = append(order, "reference"); return nil },
	)
	if err != nil {
		t.Fatalf("sampleInterleaved: %v", err)
	}
	if len(order) != 2*iterations {
		t.Fatalf("recorded %d calls, want %d", len(order), 2*iterations)
	}
	measuredFirst := 0
	for i := 0; i < len(order); i += 2 {
		if order[i] == "measured" {
			measuredFirst++
		}
	}
	if want := iterations / 2; measuredFirst != want {
		t.Fatalf("measured callback opened the pair %d times out of %d, want %d", measuredFirst, iterations, want)
	}
}

func TestSampleInterleavedCountsBothSeriesOncePerIteration(t *testing.T) {
	const iterations = 7
	measured, reference, err := sampleInterleaved(iterations,
		func() error { return nil },
		func() error { return nil },
	)
	if err != nil {
		t.Fatalf("sampleInterleaved: %v", err)
	}
	if len(measured) != iterations || len(reference) != iterations {
		t.Fatalf("sample counts = (%d, %d), want (%d, %d)", len(measured), len(reference), iterations, iterations)
	}
}

func TestSampleInterleavedSurfacesCallbackError(t *testing.T) {
	// A swallowed callback error would let a broken measurement produce a
	// confident-looking ratio, so it must abort sampling.
	wantErr := errors.New("boom")
	measured, reference, err := sampleInterleaved(5,
		func() error { return wantErr },
		func() error { return nil },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("sampleInterleaved() error = %v, want %v", err, wantErr)
	}
	if measured != nil || reference != nil {
		t.Fatalf("sample slices = (%v, %v), want both nil on error", measured, reference)
	}
}

// TestPairedRatiosCancelsCommonModeNoise is the property the NFR budgets rest
// on: when both series are scaled by the same per-iteration host factor — which
// is what CPU contention does to two calls running microseconds apart — the
// paired ratios are unchanged. That is why the budgets can stay tight enough to
// catch a regression while giving the same verdict on a busy host as an idle
// one.
func TestPairedRatiosCancelsCommonModeNoise(t *testing.T) {
	measured := []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	reference := []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}

	// Contention hits both calls in a pair equally, and differently per pair.
	contention := []time.Duration{1, 7, 40}
	loaded := make([]time.Duration, 0, len(measured))
	loadedRef := make([]time.Duration, 0, len(reference))
	for i := range measured {
		loaded = append(loaded, measured[i]*contention[i])
		loadedRef = append(loadedRef, reference[i]*contention[i])
	}

	quiet := pairedRatios(measured, reference)
	busy := pairedRatios(loaded, loadedRef)
	for i := range quiet {
		if quiet[i] != busy[i] {
			t.Fatalf("pair %d: quiet ratio %.4f != busy ratio %.4f; host load must cancel", i, quiet[i], busy[i])
		}
		if quiet[i] != 2.0 {
			t.Fatalf("pair %d ratio = %.4f, want 2.0", i, quiet[i])
		}
	}
}

// TestPairedRatiosSurfacesRegressionInOneSeries is the other half of the
// contract: a slowdown confined to the measured series must still show up.
func TestPairedRatiosSurfacesRegressionInOneSeries(t *testing.T) {
	reference := []time.Duration{5 * time.Millisecond, 5 * time.Millisecond}
	regressed := []time.Duration{50 * time.Millisecond, 50 * time.Millisecond}
	for i, got := range pairedRatios(regressed, reference) {
		if got != 10.0 {
			t.Fatalf("pair %d ratio = %.4f, want 10.0", i, got)
		}
	}
}

func TestPairedRatiosPanicsOnLengthMismatch(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("pairedRatios() did not panic on mismatched lengths")
		}
	}()
	pairedRatios([]time.Duration{time.Millisecond}, nil)
}

func TestMedianRatioHandlesOddAndEvenLengths(t *testing.T) {
	tests := []struct {
		name   string
		ratios []float64
		want   float64
	}{
		{"odd length takes the middle", []float64{5.0, 1.0, 3.0}, 3.0},
		{"even length averages the two middles", []float64{1.0, 2.0, 4.0, 8.0}, 3.0},
		{"single sample", []float64{2.5}, 2.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := medianRatio(tt.ratios); got != tt.want {
				t.Fatalf("medianRatio() = %.2f, want %.2f", got, tt.want)
			}
		})
	}
}

// TestMedianRatioResistsSinglePairOutliers is why the NFR budgets summarize
// with the median: one pair catching a scheduler preemption must not move the
// verdict, or the budget has to be loosened past the point of catching a real
// regression.
func TestMedianRatioResistsSinglePairOutliers(t *testing.T) {
	steady := []float64{1.4, 1.5, 1.5, 1.6, 1.5}
	spiked := []float64{1.4, 1.5, 1.5, 1.6, 40.0}
	if got, want := medianRatio(steady), 1.5; got != want {
		t.Fatalf("medianRatio(steady) = %.2f, want %.2f", got, want)
	}
	if got, want := medianRatio(spiked), 1.5; got != want {
		t.Fatalf("medianRatio(spiked) = %.2f, want %.2f; one outlier must not move the median", got, want)
	}
}

// TestNFRBudgetsSplitTheMeasuredDiscriminationWindow locks in the analysis
// behind the budget constants.
//
// A ratio budget is only useful if it sits strictly between what healthy code
// measures and what regressed code measures. Both bounds below were measured on
// a loaded 24-core box, not assumed — see the constants' doc comment. This test
// guards the usual failure mode from both sides: raising the budget until a red
// test goes green (until it can no longer catch anything), or tightening it
// onto the healthy band (where it fires on unmodified code).
func TestNFRBudgetsSplitTheMeasuredDiscriminationWindow(t *testing.T) {
	// Worst healthy median observed across 14 runs (NFR-1 0.90-1.12x,
	// NFR-4 0.94-1.07x), rounded up.
	const worstHealthyRatio = 1.15
	// Median observed after injecting an extra tree walk into DetectPackDrift
	// (1.93-1.98x), rounded down. Short of a clean 2x because the second walk
	// hits a warm dentry cache.
	const regressedRatio = 1.90

	for _, tt := range []struct {
		name   string
		budget float64
	}{
		{"NFR-1 pack drift", nfr1BudgetRatio},
		{"NFR-4 binary drift", nfr4BudgetRatio},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.budget <= worstHealthyRatio {
				t.Fatalf("budget %.2fx is at or below the worst healthy ratio %.2fx; it would fire on unmodified code",
					tt.budget, worstHealthyRatio)
			}
			if tt.budget >= regressedRatio {
				t.Fatalf("budget %.2fx is at or above %.2fx, the ratio a measured doubling of the path produces; it could no longer catch that regression",
					tt.budget, regressedRatio)
			}
		})
	}
}

func TestPercentile95UsesTheNFRIndexConvention(t *testing.T) {
	tests := []struct {
		name    string
		samples []time.Duration
		want    time.Duration
	}{
		{
			// The NFR briefs fix this case: for n=30 the p95 index is
			// ceil(0.95*30)-1 = 28, i.e. the second-largest sample.
			name:    "n=30 selects index 28",
			samples: rampSamples(30),
			want:    29 * time.Millisecond,
		},
		{
			name:    "single sample is its own p95",
			samples: []time.Duration{7 * time.Millisecond},
			want:    7 * time.Millisecond,
		},
		{
			name:    "n=20 selects index 19",
			samples: rampSamples(20),
			want:    20 * time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentile95(tt.samples); got != tt.want {
				t.Fatalf("percentile95() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPercentile95SortsUnorderedInput(t *testing.T) {
	// The NFR tests read min and max off the slice after calling percentile95,
	// so the in-place sort is part of the contract, not an implementation
	// detail.
	samples := []time.Duration{
		9 * time.Millisecond,
		1 * time.Millisecond,
		5 * time.Millisecond,
	}
	if got, want := percentile95(samples), 9*time.Millisecond; got != want {
		t.Fatalf("percentile95(unsorted) = %s, want %s", got, want)
	}
	if samples[0] != 1*time.Millisecond {
		t.Fatalf("samples[0] = %s, want the minimum (1ms) after the in-place sort", samples[0])
	}
	if last := samples[len(samples)-1]; last != 9*time.Millisecond {
		t.Fatalf("samples[len-1] = %s, want the maximum (9ms) after the in-place sort", last)
	}
}

func TestMeanDurationAveragesSamples(t *testing.T) {
	samples := []time.Duration{
		2 * time.Millisecond,
		4 * time.Millisecond,
		9 * time.Millisecond,
	}
	if got, want := meanDuration(samples), 5*time.Millisecond; got != want {
		t.Fatalf("meanDuration() = %s, want %s", got, want)
	}
}

// TestReferenceTreeWalkCostCoversEveryPackFile is the load-bearing assumption
// of the NFR-1 ratio budget: the denominator must traverse exactly the tree the
// numerator does. If the reference walk covered less, contention would inflate
// the two unequally and the ratio would drift with host load — reintroducing
// the flake the ratio exists to remove.
func TestReferenceTreeWalkCostCoversEveryPackFile(t *testing.T) {
	const packs, filesPerPack = 3, 8
	roots := buildRealisticPackTree(t, packs, filesPerPack)
	dirs := make([]string, 0, len(roots))
	for _, root := range roots {
		dirs = append(dirs, root.Dir)
	}

	visited, err := referenceTreeWalkCost(dirs)
	if err != nil {
		t.Fatalf("referenceTreeWalkCost: %v", err)
	}
	if want := packs * filesPerPack; visited != want {
		t.Fatalf("referenceTreeWalkCost visited %d files, want %d", visited, want)
	}
}

func TestReferenceTreeWalkCostReportsMissingRoot(t *testing.T) {
	// A silently-swallowed walk error would make the denominator ~0 and the
	// ratio explode into a false NFR violation, so the error must surface.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := referenceTreeWalkCost([]string{missing}); err == nil {
		t.Fatal("referenceTreeWalkCost() error = nil, want an error for a missing root")
	}
}

// rampSamples returns n samples of 1ms, 2ms, ... n*1ms.
func rampSamples(n int) []time.Duration {
	samples := make([]time.Duration, 0, n)
	for i := 1; i <= n; i++ {
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}
	return samples
}
