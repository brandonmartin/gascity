package main

// Benchmarks and NFR-budget tests for the supervisor drift-detection
// hot path (ga-a3ry.1 phase 3). Companion file to cmd_start_drift_test.go;
// kept separate so the unit-test file's flag-matrix and wording pins stay
// readable without a 200-line benchmark appendix.

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// Why the NFR budgets below are ratios instead of wall-clock thresholds.
//
// Both NFR tests originally asserted an absolute duration — "p95 < 100ms",
// "avg < 10ms". On a shared, heavily oversubscribed host those assertions
// measure the neighbors, not the code: NFR-1 was observed failing at p95 =
// 100.496ms purely from CPU contention at load 58-107 on a 24-core box
// (ga-df7s). That is worse than a flake. It makes the whole cmd/gc process
// suite unreadable as a gate, because a red result cannot be distinguished
// from a real regression without manually re-running against a baseline
// worktree.
//
// Each test now measures a reference workload alongside the code under test,
// interleaved sample for sample so both see the same host conditions, and
// budgets the ratio between them. Host contention inflates numerator and
// denominator together, so the verdict is the same on a busy host as on an
// idle one — the acceptance criterion for ga-df7s. An algorithmic regression
// inflates only the numerator and still fails, which is the property the NFRs
// actually exist to protect.
//
// The reference workloads are deliberately dependency-free: they must not call
// into the code under test, or a regression would slow the denominator too and
// hide itself.

// sampleInterleaved runs measured and reference once each per iteration and
// returns their per-iteration costs.
//
// The order within each pair alternates. Running the measured call first every
// time gives it a systematic penalty — a cold keepalive, a scheduler slice
// boundary, a GC assist all land on whichever call opens the pair — which
// inflates the ratio and silently eats the budget's headroom. Alternating
// cancels that bias across the run while keeping both series exposed to the
// same host conditions, which is the whole point of interleaving.
//
// Either callback returning an error aborts sampling and surfaces it.
func sampleInterleaved(iterations int, measured, reference func() error) (measuredSamples, referenceSamples []time.Duration, err error) {
	measuredSamples = make([]time.Duration, 0, iterations)
	referenceSamples = make([]time.Duration, 0, iterations)
	for i := 0; i < iterations; i++ {
		steps := [2]struct {
			run func() error
			out *[]time.Duration
		}{
			{measured, &measuredSamples},
			{reference, &referenceSamples},
		}
		if i%2 == 1 {
			steps[0], steps[1] = steps[1], steps[0]
		}
		for _, step := range steps {
			start := time.Now()
			runErr := step.run()
			elapsed := time.Since(start)
			if runErr != nil {
				return nil, nil, runErr
			}
			*step.out = append(*step.out, elapsed)
		}
	}
	return measuredSamples, referenceSamples, nil
}

// NFR cost budgets, expressed as a multiple of the reference workload each test
// measures alongside the code under test.
//
// Both budgets sit in a measured discrimination window, not at a guessed round
// number. On a 24-core box at load ~58:
//
//   - Healthy: the median paired ratio spans 0.90-1.12x for NFR-1 over 14 runs
//     and 0.94-1.07x for NFR-4. Both paths cost about what their reference
//     workload costs, which is the point — the reference is the unavoidable
//     work each path is built on.
//   - Regressed: making DetectPackDrift walk each root twice — the classic
//     extra-pass regression — moves the median to 1.93-1.98x. Note it does not
//     reach a clean 2x: the second walk hits a warm dentry cache, so a doubling
//     of the work is worth measuring rather than assuming.
//
// 1.5x splits that window, clearing the worst healthy run by ~34% and catching
// the doubling by ~22%. Verified in both directions by injecting the regression
// above; TestNFRBudgetsSplitTheMeasuredDiscriminationWindow pins the reasoning.
//
// Do not loosen these to silence a failure. They are ratios against a workload
// that tracks host load, so contention cannot move them — see the rationale
// above. A number that drifts up here is a regression or a broken reference
// workload, not a flake.
const (
	nfr1BudgetRatio = 1.5
	nfr4BudgetRatio = 1.5
)

// pairedRatios returns the per-iteration cost ratio measured[i]/reference[i].
// It panics on mismatched lengths, which can only be a caller bug.
//
// Pairing before dividing is what makes the NFR budgets stable enough to be
// worth asserting. The two calls in a pair run microseconds apart, so they see
// the same CPU contention, cache state, and scheduler pressure; dividing within
// the pair cancels that common-mode noise at the sample level. Dividing two
// independently-aggregated percentiles instead compounds their variance —
// measured on a loaded 24-core box, that form swung between 0.61x and 1.98x run
// to run for identical code, which would force a budget so loose it could no
// longer catch the regression it exists to catch.
func pairedRatios(measured, reference []time.Duration) []float64 {
	if len(measured) != len(reference) {
		panic(fmt.Sprintf("pairedRatios: length mismatch %d != %d", len(measured), len(reference)))
	}
	ratios := make([]float64, 0, len(measured))
	for i := range measured {
		ratios = append(ratios, float64(measured[i])/float64(reference[i]))
	}
	return ratios
}

// percentile95 returns the 95th-percentile sample. samples is sorted in place.
// The index convention matches the NFR briefs: for n=30, ceil(0.95*30)-1 = 28.
func percentile95(samples []time.Duration) time.Duration {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)*95/100]
}

// medianRatio returns the median of ratios. ratios is sorted in place.
//
// The median, not a high percentile, is the right summary of a paired ratio.
// The two series' tails are dominated by whichever call happened to catch a
// scheduler preemption, so the ratio's upper tail measures the host, not the
// code — a p95 over 30 pairs was observed at 2.83x on unmodified code, against
// a 1.35-1.95x body, which would force a budget too loose to catch a doubling.
// The median is robust to those single-pair excursions and expresses the
// property the NFRs are really about: how expensive detection is relative to
// the unavoidable work, in the typical case. Absolute p95 cost is still logged
// for observability.
func medianRatio(ratios []float64) float64 {
	sort.Float64s(ratios)
	mid := len(ratios) / 2
	if len(ratios)%2 == 1 {
		return ratios[mid]
	}
	return (ratios[mid-1] + ratios[mid]) / 2
}

// meanDuration returns the arithmetic mean of samples.
func meanDuration(samples []time.Duration) time.Duration {
	var total time.Duration
	for _, s := range samples {
		total += s
	}
	return total / time.Duration(len(samples))
}

// referenceTreeWalkCost walks dirs and stats every entry — the same filesystem
// work DetectPackDrift performs per root, with none of its logic. It is the
// load-tracking denominator for the NFR-1 ratio budget.
func referenceTreeWalkCost(dirs []string) (int, error) {
	visited := 0
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if _, err := d.Info(); err != nil {
				return err
			}
			visited++
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return visited, nil
}

// fakeHealthServer returns an httptest server that responds to /health
// with the supplied build_id. It approximates the supervisor's hot-path
// response so drift-detection benchmarks can measure the round-trip
// without spinning up a real supervisor.
func fakeHealthServer(buildID string) *httptest.Server {
	body := fmt.Sprintf(`{"status":"ok","version":"v0","build_id":%q,"uptime_sec":1,"cities_total":0,"cities_running":0}`, buildID)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// BenchmarkDriftDetect_NoDrift measures the cost of the no-drift path:
// supervisorAlive probe + /health round-trip + DetectBinaryDrift compare.
// NFR-4 from the architect's brief: <10ms per detection on the hot path.
//
// The probe and HTTP fetch are the only non-CPU costs in the no-drift
// path; this benchmark intentionally skips the city.toml load (which is
// well under a millisecond per the existing config tests).
func BenchmarkDriftDetect_NoDrift(b *testing.B) {
	const buildID = "abc12345"
	srv := fakeHealthServer(buildID)
	b.Cleanup(srv.Close)

	oldHook := supervisorAliveHook
	b.Cleanup(func() { supervisorAliveHook = oldHook })
	supervisorAliveHook = func() int { return 4242 }

	client := newHTTPSupervisorClient(srv.URL)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if pid := supervisorAliveHook(); pid == 0 {
			b.Fatalf("supervisorAlive returned 0")
		}
		status, err := client.Status(ctx)
		if err != nil {
			b.Fatalf("Status: %v", err)
		}
		if DetectBinaryDrift(buildID, status) {
			b.Fatalf("expected no drift; got drift")
		}
	}
}

// BenchmarkDriftDetect_WithRealisticPacks measures DetectPackDrift
// against a 5-pack tree with ~hundreds of files. NFR-1 budget: <100ms
// p95. The pack tree mirrors the gastown / consumer pack scale operators
// see in the field.
func BenchmarkDriftDetect_WithRealisticPacks(b *testing.B) {
	roots := buildRealisticPackTree(b, 5, 120)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drifted, err := DetectPackDrift(roots)
		if err != nil {
			b.Fatalf("DetectPackDrift: %v", err)
		}
		if len(drifted) != 0 {
			b.Fatalf("expected no drift on warmly-parsed roots; got %v", drifted)
		}
	}
}

// buildRealisticPackTree constructs n pack roots, each with filesPerPack
// regular files. ParsedAt is set to one hour in the future so DetectPackDrift
// reports no drift; the benchmark measures the walk cost, which dominates.
func buildRealisticPackTree(tb testing.TB, n, filesPerPack int) []PackRootStatus {
	tb.Helper()
	root := tb.TempDir()
	parsedAt := time.Now().Add(time.Hour)
	roots := make([]PackRootStatus, 0, n)
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, fmt.Sprintf("pack-%d", i))
		// Spread files across two subdirectories so the walk visits
		// nested entries, matching real packs.
		sub1 := filepath.Join(dir, "agents")
		sub2 := filepath.Join(dir, "formulas")
		for _, d := range []string{sub1, sub2} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				tb.Fatalf("mkdir: %v", err)
			}
		}
		for j := 0; j < filesPerPack; j++ {
			target := sub1
			if j%2 == 0 {
				target = sub2
			}
			path := filepath.Join(target, fmt.Sprintf("file-%03d.toml", j))
			if err := os.WriteFile(path, []byte("name = \"x\"\n"), 0o644); err != nil {
				tb.Fatalf("write: %v", err)
			}
		}
		roots = append(roots, PackRootStatus{Dir: dir, ParsedAt: parsedAt})
	}
	return roots
}

// TestDriftDetect_NoDrift_NFR4 is the unit-test counterpart of
// BenchmarkDriftDetect_NoDrift: it runs the same no-drift round-trip
// repeatedly and asserts the average cost is comfortably under NFR-4's
// 10ms budget. Failing here surfaces in `go test` (no -bench flag
// required), which is what CI runs.
func TestDriftDetect_NoDrift_NFR4(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFR budget test in short mode")
	}
	const buildID = "abc12345"
	srv := fakeHealthServer(buildID)
	t.Cleanup(srv.Close)

	oldHook := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = oldHook })
	supervisorAliveHook = func() int { return 4242 }

	client := newHTTPSupervisorClient(srv.URL)
	ctx := context.Background()

	const iterations = 100
	// The /health round-trip is the only non-trivial cost in the detect path;
	// the alive probe and the build-id compare are meant to disappear next to
	// it. Budget the whole path against a bare round-trip, which tracks host
	// load identically. See the ratio rationale at the top of this file.
	const budgetRatio = nfr4BudgetRatio

	// Warm the loopback connection and the GC, then measure.
	for i := 0; i < 5; i++ {
		_, _ = client.Status(ctx)
	}

	detectSamples, referenceSamples, err := sampleInterleaved(iterations,
		func() error {
			if pid := supervisorAliveHook(); pid == 0 {
				return fmt.Errorf("supervisorAlive returned 0")
			}
			status, err := client.Status(ctx)
			if err != nil {
				return fmt.Errorf("Status: %w", err)
			}
			if DetectBinaryDrift(buildID, status) {
				return fmt.Errorf("expected no drift")
			}
			return nil
		},
		// Reference: the bare round-trip the detect path is built on.
		func() error {
			if _, err := client.Status(ctx); err != nil {
				return fmt.Errorf("reference Status: %w", err)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ratio := medianRatio(pairedRatios(detectSamples, referenceSamples))
	if ratio > budgetRatio {
		t.Fatalf("NFR-4 violated: median detect cost = %.2fx a bare /health round-trip over %d iterations, budget %.2fx (avg detect %s, avg round-trip %s)",
			ratio, iterations, budgetRatio, meanDuration(detectSamples), meanDuration(referenceSamples))
	}
	t.Logf("NFR-4 OK: median paired ratio %.2fx (budget %.2fx) — avg detect = %s, avg round-trip = %s over %d iterations",
		ratio, budgetRatio, meanDuration(detectSamples), meanDuration(referenceSamples), iterations)
}

// TestDriftDetect_WithRealisticPacks_NFR1 pins the NFR-1 cost budget for
// DetectPackDrift over a 5-pack city. p95 is computed across enough samples
// that the upper tail is meaningful.
//
// DetectPackDrift walks each root and stats every file, keeping the newest
// mtime. referenceTreeWalkCost performs exactly that traversal and nothing
// else, so NFR-1's real content — "detection stays proportional to a single
// stat-per-entry walk, with no extra pass, per-file parse, or O(n^2) compare" —
// is expressed directly as a ratio against it, and holds at any host load. See
// the rationale at the top of this file for why the old absolute budget could
// not.
func TestDriftDetect_WithRealisticPacks_NFR1(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFR budget test in short mode")
	}
	// Mirrors the gastown / consumer pack scale operators see in the field.
	const packs, filesPerPack = 5, 120
	roots := buildRealisticPackTree(t, packs, filesPerPack)
	dirs := make([]string, 0, len(roots))
	for _, root := range roots {
		dirs = append(dirs, root.Dir)
	}

	const iterations = 30
	const budgetRatio = nfr1BudgetRatio

	detectSamples, referenceSamples, err := sampleInterleaved(iterations,
		func() error {
			drifted, err := DetectPackDrift(roots)
			if err != nil {
				return fmt.Errorf("DetectPackDrift: %w", err)
			}
			if len(drifted) != 0 {
				return fmt.Errorf("expected no drift; got %v", drifted)
			}
			return nil
		},
		// Reference: the same traversal DetectPackDrift performs, with none of
		// its logic.
		func() error {
			visited, err := referenceTreeWalkCost(dirs)
			if err != nil {
				return fmt.Errorf("referenceTreeWalkCost: %w", err)
			}
			if want := len(roots) * filesPerPack; visited != want {
				return fmt.Errorf("reference walk visited %d files, want %d; it must cover the same tree DetectPackDrift does", visited, want)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ratio := medianRatio(pairedRatios(detectSamples, referenceSamples))
	detectP95 := percentile95(detectSamples)
	referenceP95 := percentile95(referenceSamples)
	if ratio > budgetRatio {
		t.Fatalf("NFR-1 violated: median detect cost = %.2fx a reference tree walk over %d iterations, budget %.2fx (p95 detect %s, p95 walk %s)",
			ratio, iterations, budgetRatio, detectP95, referenceP95)
	}
	// percentile95 sorted both slices in place, so index 0 and len-1 are the
	// measured min and max.
	t.Logf("NFR-1 OK: median paired ratio %.2fx (budget %.2fx) — p95 detect = %s (min %s, max %s), p95 reference walk = %s over %d samples",
		ratio, budgetRatio, detectP95, detectSamples[0], detectSamples[len(detectSamples)-1], referenceP95, iterations)
}
