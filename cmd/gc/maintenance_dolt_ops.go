package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// maintenanceDoltUser is the SQL user the maintenance loop connects as. The
// managed Dolt server authenticates the controller as root, matching the
// rest of the managed-Dolt lifecycle (see beads_provider_lifecycle.go).
const maintenanceDoltUser = "root"

// maintenanceDoltSmokeTable is the bd-managed table the post-GC smoke read
// counts. bd's Dolt schema names it "issues" (the same literal the
// supervisor smoke probe and internal/api/convoy_sql.go use).
const maintenanceDoltSmokeTable = "issues"

// newMaintenanceDoltOpsFactory returns a supervisor.DoltOpsFactory that, on
// each maintenance cycle, resolves the city's managed Dolt port and opens a
// per-cycle DoltOps which runs CALL DOLT_GC() across every user database.
//
// Resolution is lazy (per cycle) so a Dolt server that is down or has not
// published its port yet surfaces as a stage="gc" failure on that run —
// loudly recorded and alerted — rather than a silent no-op. This is the fix
// for ga-ule: production previously left OpenDoltOps nil, so runDoltGC
// returned immediately and the cycle reported stage=done in 0.0s while
// reclaiming nothing.
func newMaintenanceDoltOpsFactory(cityPath string) supervisor.DoltOpsFactory {
	return func(_ context.Context) (supervisor.DoltOps, error) {
		port := strings.TrimSpace(currentResolvableManagedDoltPort(cityPath))
		if port == "" {
			return nil, fmt.Errorf("managed Dolt port is not resolvable for city %s (server down or runtime state not published)", cityPath)
		}
		return &maintenanceDoltOps{
			host: managedDoltConnectHost(""),
			port: port,
			user: maintenanceDoltUser,
		}, nil
	}
}

// maintenanceDoltOps implements supervisor.DoltOps against the managed Dolt
// server. Dolt's CALL DOLT_GC() collects exactly one database — the one
// selected on the connection — so ExecGC enumerates every user database and
// garbage-collects each in turn. This mirrors the proven manual remediation
// (`gc dolt sql -q "USE <db>; CALL DOLT_GC();"` per database) rather than a
// single-database CALL DOLT_GC() that would leave the other city databases
// unreclaimed.
type maintenanceDoltOps struct {
	host, port, user string

	// runSQL shells out to `dolt sql` bounded by the supplied context. A
	// field (rather than a direct call) so tests inject a fake without a
	// live Dolt server; nil uses the production runner.
	runSQL func(ctx context.Context, args ...string) (string, error)
}

func (o *maintenanceDoltOps) exec(ctx context.Context, args ...string) (string, error) {
	if o.runSQL != nil {
		return o.runSQL(ctx, args...)
	}
	return runManagedDoltSQLCtxBounded(ctx, o.host, o.port, o.user, args...)
}

// userDatabases returns the non-system databases visible on the managed
// Dolt server. The SHOW DATABASES probe is short; it shares the cycle's GC
// deadline via ctx.
func (o *maintenanceDoltOps) userDatabases(ctx context.Context) ([]string, error) {
	out, err := o.exec(ctx, "-r", "csv", "-q", "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	return managedDoltUserDatabasesFromCSV(out)
}

// ExecGC runs CALL DOLT_GC() against every user database. A run with zero
// user databases is reported as an error rather than a silent success so an
// empty or unreachable catalog cannot masquerade as a completed GC.
func (o *maintenanceDoltOps) ExecGC(ctx context.Context) error {
	dbs, err := o.userDatabases(ctx)
	if err != nil {
		return fmt.Errorf("list databases: %w", err)
	}
	if len(dbs) == 0 {
		return fmt.Errorf("no user databases found on managed Dolt server %s:%s", o.host, o.port)
	}
	for _, db := range dbs {
		// USE + CALL DOLT_GC() run in a single `dolt sql` invocation: each
		// invocation is a fresh client connection, so DOLT_GC's
		// connection-closing behavior cannot wedge a pooled connection for
		// the next database.
		query := "USE " + managedDoltQuoteIdent(db) + "; CALL DOLT_GC();"
		if _, err := o.exec(ctx, "-q", query); err != nil {
			return fmt.Errorf("CALL DOLT_GC() on database %q: %w", db, err)
		}
	}
	return nil
}

// SmokeCount verifies the store is readable after GC by counting the
// bd-managed table across every user database and returning the sum. A
// database that lacks the table contributes nothing (it is not necessarily
// a beads store); any other read error propagates so a wedged or read-only
// server is reported as a smoke-test failure.
func (o *maintenanceDoltOps) SmokeCount(ctx context.Context) (int, error) {
	dbs, err := o.userDatabases(ctx)
	if err != nil {
		return 0, fmt.Errorf("list databases: %w", err)
	}
	total := 0
	for _, db := range dbs {
		query := "USE " + managedDoltQuoteIdent(db) + "; SELECT COUNT(*) AS cnt FROM " + managedDoltQuoteIdent(maintenanceDoltSmokeTable) + ";"
		out, err := o.exec(ctx, "-r", "csv", "-q", query)
		if err != nil {
			if isMissingTableErr(err) {
				continue
			}
			return 0, fmt.Errorf("smoke read on database %q: %w", db, err)
		}
		n, perr := parseSmokeCount(out)
		if perr != nil {
			return 0, fmt.Errorf("smoke read on database %q: %w", db, perr)
		}
		total += n
	}
	return total, nil
}

// Close is a no-op: each SQL call is an independent `dolt sql` invocation,
// so there is no persistent connection to release.
func (o *maintenanceDoltOps) Close() error { return nil }

// isMissingTableErr reports whether err is Dolt's "table not found" — the
// one read failure SmokeCount tolerates, since a user database without the
// bd-managed table simply is not a beads store.
func isMissingTableErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "table not found")
}

// parseSmokeCount extracts the integer from `SELECT COUNT(*)` csv output by
// taking the last numeric line (the value row, after the "cnt" header).
func parseSmokeCount(out string) (int, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil {
			return 0, fmt.Errorf("parse smoke count %q: %w", line, err)
		}
		return n, nil
	}
	return 0, fmt.Errorf("empty smoke count output")
}

// runManagedDoltSQLCtxBounded shells out to `dolt sql` against the managed
// Dolt server, bounded only by ctx — no inner timeout is applied, so a long
// CALL DOLT_GC() on a large database runs to completion under the
// maintenance loop's own GC deadline. It mirrors the managed-Dolt SQL client
// invocation used elsewhere in this package; it is kept self-contained here
// so the upstream-owned dolt_sql_health.go (whose runner caps every call at
// a few seconds) needs no edit.
func runManagedDoltSQLCtxBounded(ctx context.Context, host, port, user string, args ...string) (string, error) {
	host = managedDoltConnectHost(host)
	port = strings.TrimSpace(port)
	if port == "" {
		return "", fmt.Errorf("missing port")
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = maintenanceDoltUser
	}
	baseArgs := []string{
		"--host", host,
		"--port", port,
		"--user", user,
		"--password", managedDoltPassword(),
		"--no-tls",
		"sql",
	}
	cmd := exec.CommandContext(ctx, "dolt", append(baseArgs, args...)...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("dolt sql timed out: %s", msg)
		}
		return "", fmt.Errorf("dolt sql timed out")
	}
	if err == nil {
		return string(out), nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return "", err
	}
	return "", fmt.Errorf("%s", msg)
}
