package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawHangDeadlinePattern is ga-57b2dk's acceptance-check regex: the raw-
// literal-duration shapes that #4638/#4639 replaced with awaitClose (for a
// channel drain) or awaitCond (for a polled condition) everywhere else in
// this package's cmd/gc tests. The third alternative catches
// waitForNamedMode's two call-site literals, which are invisible to the
// first two alternatives because the raw duration is an argument rather than
// a direct time.After/time.Now().Add call.
var rawHangDeadlinePattern = regexp.MustCompile(`time\.After\([0-9]|time\.Now\(\)\.Add\([0-9]|waitForNamedMode\([^)]*,\s*[0-9]`)

const rawHangDeadlineExclusionMarker = "(ga-57b2dk exclusion)"

func controllerTestPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootForLint(t), "cmd", "gc", "controller_test.go")
}

func controllerTestLines(t *testing.T) (path string, data []byte, lines []string) {
	t.Helper()
	path = controllerTestPath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return path, data, strings.Split(string(data), "\n")
}

// rawHangDeadlineExclusionPrecedes reports whether the closest preceding
// non-blank line marks this raw deadline as an intentional exclusion.
func rawHangDeadlineExclusionPrecedes(lines []string, lineIndex int) bool {
	for i := lineIndex - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		return strings.Contains(lines[i], rawHangDeadlineExclusionMarker)
	}
	return false
}

// rawHangDeadlineExclusionMarkerErrors reports marker comments that no longer
// lead to a raw deadline, so exclusions cannot outlive the code they explain.
func rawHangDeadlineExclusionMarkerErrors(path string, lines []string) []string {
	var stale []string
	for i, line := range lines {
		if !strings.Contains(line, rawHangDeadlineExclusionMarker) {
			continue
		}
		next := i + 1
		for next < len(lines) && strings.TrimSpace(lines[next]) == "" {
			next++
		}
		if next == len(lines) || !rawHangDeadlinePattern.MatchString(lines[next]) {
			stale = append(stale, fmt.Sprintf("%s:%d: exclusion marker is not followed by a raw hang deadline", path, i+1))
		}
	}
	return stale
}

// rawHangDeadlineOffenders returns formatted offender strings for every line
// in [from, to] that matches rawHangDeadlinePattern without an exclusion
// marker on the preceding non-blank line.
func rawHangDeadlineOffenders(path string, lines []string, from, to int) []string {
	var offenders []string
	if from < 1 {
		from = 1
	}
	for i := from; i <= to && i <= len(lines); i++ {
		line := lines[i-1]
		if !rawHangDeadlinePattern.MatchString(line) {
			continue
		}
		if rawHangDeadlineExclusionPrecedes(lines, i-1) {
			continue
		}
		offenders = append(offenders, formatOffender(path, i, line))
	}
	return offenders
}

func TestRawHangDeadlineExclusionUsesCommentNotLineNumber(t *testing.T) {
	lines := []string{
		"package example",
		"",
		"// This wait defines the scenario (ga-57b2dk exclusion).",
		"",
		"<-time.After(50 * time.Millisecond)",
	}
	lines = append([]string{"", "", ""}, lines...)

	if offenders := rawHangDeadlineOffenders("controller_test.go", lines, 1, len(lines)); len(offenders) != 0 {
		t.Fatalf("marked raw hang deadline should be excluded after line shifts, got %v", offenders)
	}
}

func TestRawHangDeadlineExclusionMarkerMustRemainCurrent(t *testing.T) {
	valid := []string{
		"// This wait defines the scenario (ga-57b2dk exclusion).",
		"",
		"<-time.After(50 * time.Millisecond)",
	}
	if stale := rawHangDeadlineExclusionMarkerErrors("controller_test.go", valid); len(stale) != 0 {
		t.Fatalf("current exclusion marker reported stale: %v", stale)
	}

	for name, lines := range map[string][]string{
		"deadline removed": {
			"// This wait defines the scenario (ga-57b2dk exclusion).",
			"",
			"awaitClose(t, done, \"operation completed\")",
		},
		"file ends after marker": {
			"// This wait defines the scenario (ga-57b2dk exclusion).",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if stale := rawHangDeadlineExclusionMarkerErrors("controller_test.go", lines); len(stale) != 1 {
				t.Fatalf("expected one stale marker error, got %v", stale)
			}
		})
	}
}

// TestControllerTestHasNoUnmigratedRawHangDeadlines pins ga-57b2dk's primary
// acceptance check: every sub-10s raw-literal timer in controller_test.go
// without a marker-documented exclusion must be migrated to
// awaitClose/awaitCond, exactly as #4638 already did for the rest of the
// package.
func TestControllerTestHasNoUnmigratedRawHangDeadlines(t *testing.T) {
	path, _, lines := controllerTestLines(t)

	offenders := rawHangDeadlineOffenders(path, lines, 1, len(lines))
	if len(offenders) > 0 {
		t.Fatalf("controller_test.go has %d raw-literal hang deadline(s); replace with awaitClose "+
			"(channel drain) or awaitCond (polled condition) per ga-57b2dk:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}

	// Guard against an exclusion marker surviving after its raw deadline was
	// removed or migrated.
	if stale := rawHangDeadlineExclusionMarkerErrors(path, lines); len(stale) > 0 {
		t.Errorf("controller_test.go has stale raw hang deadline exclusion marker(s):\n  %s",
			strings.Join(stale, "\n  "))
	}

	shifted := append([]string{"", "", ""}, lines...)
	if offenders := rawHangDeadlineOffenders(path, shifted, 1, len(shifted)); len(offenders) > 0 {
		t.Errorf("leading blank lines changed the raw hang deadline exclusions:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestControllerTestNoFunctionMixesHangBudgetWithRawDeadline pins ga-57b2dk's
// second acceptance check: a test function that already uses hangBudget for
// some of its waits must not also carry an unmigrated raw-literal deadline —
// the exact same-function inconsistency #4638 left behind in four functions.
func TestControllerTestNoFunctionMixesHangBudgetWithRawDeadline(t *testing.T) {
	path, data, lines := controllerTestLines(t)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var violations []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Pos()).Line
		end := fset.Position(fn.End()).Line

		usesHangBudget := false
		for i := start; i <= end && i <= len(lines); i++ {
			if strings.Contains(lines[i-1], "hangBudget") {
				usesHangBudget = true
				break
			}
		}
		if !usesHangBudget {
			continue
		}
		if offenders := rawHangDeadlineOffenders(path, lines, start, end); len(offenders) > 0 {
			violations = append(violations, fmt.Sprintf("%s: %s", fn.Name.Name, strings.Join(offenders, "; ")))
		}
	}

	if len(violations) > 0 {
		t.Fatalf("functions mix hangBudget with a raw-literal hang deadline (ga-57b2dk):\n  %s",
			strings.Join(violations, "\n  "))
	}
}
