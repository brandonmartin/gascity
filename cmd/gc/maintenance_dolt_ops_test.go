package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingDoltSQL builds a fake `dolt sql` runner for maintenanceDoltOps.
// It records every query it sees and dispatches on the statement so a test
// can script SHOW DATABASES / CALL DOLT_GC() / SELECT COUNT(*) independently
// without a live Dolt server.
type recordingDoltSQL struct {
	queries      []string
	showDBs      string            // CSV returned for SHOW DATABASES
	gcErr        map[string]error  // per-db CALL DOLT_GC() error
	countByDB    map[string]string // per-db SELECT COUNT csv output
	countErrByDB map[string]error  // per-db SELECT COUNT error
}

func (r *recordingDoltSQL) run(_ context.Context, args ...string) (string, error) {
	q := queryArg(args)
	r.queries = append(r.queries, q)
	switch {
	case strings.Contains(q, "SHOW DATABASES"):
		return r.showDBs, nil
	case strings.Contains(q, "CALL DOLT_GC()"):
		db := dbFromUse(q)
		if err := r.gcErr[db]; err != nil {
			return "", err
		}
		return "", nil
	case strings.Contains(q, "COUNT(*)"):
		db := dbFromUse(q)
		if err := r.countErrByDB[db]; err != nil {
			return "", err
		}
		if out, ok := r.countByDB[db]; ok {
			return out, nil
		}
		return "cnt\n0\n", nil
	default:
		return "", nil
	}
}

// queryArg returns the value following the last -q flag in args.
func queryArg(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-q" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// dbFromUse extracts the backtick-quoted database name from a leading
// USE statement, e.g. "USE `hq`; CALL DOLT_GC();" -> "hq".
func dbFromUse(q string) string {
	start := strings.Index(q, "`")
	if start < 0 {
		return ""
	}
	end := strings.Index(q[start+1:], "`")
	if end < 0 {
		return ""
	}
	return q[start+1 : start+1+end]
}

func newTestDoltOps(fake *recordingDoltSQL) *maintenanceDoltOps {
	return &maintenanceDoltOps{
		host:   "127.0.0.1",
		port:   "3307",
		user:   "root",
		runSQL: fake.run,
	}
}

func TestMaintenanceDoltOps_ExecGC_RunsPerUserDatabase(t *testing.T) {
	fake := &recordingDoltSQL{
		showDBs: "Database\ninformation_schema\nmysql\nhq\ngp\nca\n",
	}
	ops := newTestDoltOps(fake)

	if err := ops.ExecGC(context.Background()); err != nil {
		t.Fatalf("ExecGC: %v", err)
	}

	gcd := map[string]bool{}
	for _, q := range fake.queries {
		if strings.Contains(q, "CALL DOLT_GC()") {
			gcd[dbFromUse(q)] = true
		}
	}
	for _, want := range []string{"hq", "gp", "ca"} {
		if !gcd[want] {
			t.Errorf("CALL DOLT_GC() never ran for user database %q; queries=%v", want, fake.queries)
		}
	}
	if gcd["information_schema"] || gcd["mysql"] {
		t.Errorf("CALL DOLT_GC() ran against a system database; queries=%v", fake.queries)
	}
}

func TestMaintenanceDoltOps_ExecGC_NoUserDatabasesIsLoudError(t *testing.T) {
	fake := &recordingDoltSQL{showDBs: "Database\ninformation_schema\nmysql\ndolt\n"}
	ops := newTestDoltOps(fake)

	err := ops.ExecGC(context.Background())
	if err == nil {
		t.Fatal("ExecGC returned nil for a server with no user databases; want a loud error")
	}
	if !strings.Contains(err.Error(), "no user databases") {
		t.Errorf("error = %q, want it to mention 'no user databases'", err.Error())
	}
}

func TestMaintenanceDoltOps_ExecGC_PropagatesPerDatabaseError(t *testing.T) {
	fake := &recordingDoltSQL{
		showDBs: "Database\nhq\ngp\n",
		gcErr:   map[string]error{"gp": errors.New("disk full")},
	}
	ops := newTestDoltOps(fake)

	err := ops.ExecGC(context.Background())
	if err == nil {
		t.Fatal("ExecGC returned nil despite a per-database GC failure")
	}
	if !strings.Contains(err.Error(), "gp") || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error = %q, want it to name database 'gp' and the cause", err.Error())
	}
}

func TestMaintenanceDoltOps_SmokeCount_SumsAcrossDatabases(t *testing.T) {
	fake := &recordingDoltSQL{
		showDBs:   "Database\nhq\ngp\n",
		countByDB: map[string]string{"hq": "cnt\n40\n", "gp": "cnt\n2\n"},
	}
	ops := newTestDoltOps(fake)

	n, err := ops.SmokeCount(context.Background())
	if err != nil {
		t.Fatalf("SmokeCount: %v", err)
	}
	if n != 42 {
		t.Errorf("SmokeCount = %d, want 42 (40+2)", n)
	}
}

func TestMaintenanceDoltOps_SmokeCount_ToleratesMissingTable(t *testing.T) {
	fake := &recordingDoltSQL{
		showDBs:      "Database\nhq\nscratch\n",
		countByDB:    map[string]string{"hq": "cnt\n7\n"},
		countErrByDB: map[string]error{"scratch": errors.New("table not found: issues")},
	}
	ops := newTestDoltOps(fake)

	n, err := ops.SmokeCount(context.Background())
	if err != nil {
		t.Fatalf("SmokeCount: %v", err)
	}
	if n != 7 {
		t.Errorf("SmokeCount = %d, want 7 (scratch's missing table skipped)", n)
	}
}

func TestMaintenanceDoltOps_SmokeCount_PropagatesRealError(t *testing.T) {
	fake := &recordingDoltSQL{
		showDBs:      "Database\nhq\n",
		countErrByDB: map[string]error{"hq": errors.New("connection refused")},
	}
	ops := newTestDoltOps(fake)

	if _, err := ops.SmokeCount(context.Background()); err == nil {
		t.Fatal("SmokeCount returned nil for a connection error; want propagation")
	}
}
