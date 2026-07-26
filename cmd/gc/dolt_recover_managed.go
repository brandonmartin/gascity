package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type managedDoltRecoverReport struct {
	DiagnosedReadOnly bool
	HadPID            bool
	Forced            bool
	Ready             bool
	PID               int
	Port              int
	Healthy           bool
	Restarted         bool
}

type managedDoltRecoveryOps struct {
	queryProbe       func(host, port, user string) error
	healthCheck      func(host, port, user string) (managedDoltSQLHealthReport, error)
	stop             func(cityPath, port string) (managedDoltStopReport, error)
	preflightCleanup func(cityPath string) error
	start            func(cityPath, host, port, user, logLevel string, timeout time.Duration) (managedDoltStartReport, error)
	publish          func(cityPath string) error
	failedCleanup    func(cityPath string, pid, port int, cause error) error
}

func defaultManagedDoltRecoveryOps() managedDoltRecoveryOps {
	return managedDoltRecoveryOps{
		queryProbe: managedDoltQueryProbe,
		healthCheck: func(host, port, user string) (managedDoltSQLHealthReport, error) {
			return managedDoltHealthCheck(host, port, user, true)
		},
		stop: func(cityPath, port string) (managedDoltStopReport, error) {
			return stopManagedDoltProcessWithOptions(cityPath, port, false)
		},
		preflightCleanup: managedDoltPreflightCleanupFn,
		start: func(cityPath, host, port, user, logLevel string, timeout time.Duration) (managedDoltStartReport, error) {
			return startManagedDoltProcessWithOptions(cityPath, host, port, user, logLevel, -1, timeout, false)
		},
		publish:       publishManagedDoltRuntimeStateIfOwned,
		failedCleanup: cleanupFailedManagedDoltRecovery,
	}
}

// managedDoltRecoverSettleInterval is how long a queued recovery waits between
// probes while another process's replacement server finishes opening its
// databases.
const managedDoltRecoverSettleInterval = 250 * time.Millisecond

// managedDoltRecoverListenerReachableFn reports whether anything is still
// answering on the managed Dolt listener. Overridable in tests.
var managedDoltRecoverListenerReachableFn = managedDoltTCPReachable

func recoverManagedDoltProcess(cityPath, host, port, user, logLevel string, timeout time.Duration) (managedDoltRecoverReport, error) {
	return recoverManagedDoltProcessWithOps(cityPath, host, port, user, logLevel, timeout, defaultManagedDoltRecoveryOps())
}

func recoverManagedDoltProcessWithOps(cityPath, host, port, user, logLevel string, timeout time.Duration, ops managedDoltRecoveryOps) (managedDoltRecoverReport, error) {
	if strings.TrimSpace(cityPath) == "" {
		return managedDoltRecoverReport{}, fmt.Errorf("missing city path")
	}
	if strings.TrimSpace(port) == "" {
		return managedDoltRecoverReport{}, fmt.Errorf("missing port")
	}
	host = normalizeManagedDoltBindHost(host)
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	if strings.TrimSpace(logLevel) == "" {
		logLevel = "warning"
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	deadline := time.Now().Add(timeout)
	report := managedDoltRecoverReport{}
	lockFile, layout, err := openManagedDoltLifecycleLock(cityPath)
	if err != nil {
		return report, err
	}
	defer func() {
		if lockFile != nil {
			_ = lockFile.Close()
		}
	}()
	locked, err := tryManagedDoltLifecycleLock(lockFile)
	if err != nil {
		return report, err
	}
	// queued records that this recovery only got the lock because another gc
	// process just finished its own managed-Dolt lifecycle.
	queued := false
	if !locked {
		observed, acquired, waitErr := waitForManagedDoltLifecycleOrReady(cityPath, host, port, user, timeout, lockFile, layout, &report)
		if waitErr != nil {
			return report, waitErr
		}
		if observed {
			if err := ops.publish(cityPath); err != nil {
				return report, fmt.Errorf("publish managed dolt runtime state: %w", err)
			}
			lockFile = nil
			return report, nil
		}
		locked = acquired
		queued = acquired
	}
	if !locked {
		return report, fmt.Errorf("managed dolt lifecycle lock not acquired")
	}
	defer releaseManagedDoltLifecycleLock(lockFile)
	lockFile = nil

	if parsedPort, parseErr := strconv.Atoi(strings.TrimSpace(port)); parseErr == nil {
		report.Port = parsedPort
	}

	if recoverManagedDoltObservedRebindPossible(cityPath, port, pidAlive) {
		if ready := observeExistingManagedDoltForRecovery(cityPath, host, port, user, recoverManagedDoltExistingObserveTimeout(timeout), &report); ready && recoverManagedDoltShouldReuseExisting(report.Port, port) {
			report.Ready = true
			report.Healthy = true
			if err := ops.publish(cityPath); err != nil {
				return report, fmt.Errorf("publish managed dolt runtime state: %w", err)
			}
			return report, nil
		}
	}

	adopted, err := recoverManagedDoltAdoptRunningServer(cityPath, host, port, user, deadline, queued, ops, &report)
	if err != nil {
		return report, err
	}
	if adopted {
		return report, nil
	}

	stopReport, stopErr := ops.stop(cityPath, port)
	report.HadPID = stopReport.HadPID
	report.Forced = stopReport.Forced
	if stopReport.PID > 0 {
		report.PID = stopReport.PID
	}
	// Match shell recover semantics: stop is best-effort before restart.
	_ = stopErr

	if err := ops.preflightCleanup(cityPath); err != nil {
		return report, ops.failedCleanup(cityPath, report.PID, report.Port, err)
	}

	startReport, err := ops.start(cityPath, host, port, user, logLevel, timeout)
	report.Restarted = true
	report.Ready = startReport.Ready
	if startReport.PID > 0 {
		report.PID = startReport.PID
	}
	if startReport.Port > 0 {
		report.Port = startReport.Port
	} else if portNum, parseErr := strconv.Atoi(strings.TrimSpace(port)); parseErr == nil {
		report.Port = portNum
	}
	if err != nil {
		return report, err
	}

	health, err := ops.healthCheck(host, strconv.Itoa(report.Port), user)
	if err != nil {
		return report, ops.failedCleanup(cityPath, report.PID, report.Port, err)
	}
	if health.ReadOnly == "true" {
		report.Healthy = false
		return report, ops.failedCleanup(cityPath, report.PID, report.Port, fmt.Errorf("dolt server on %s:%d is still read-only after recovery", managedDoltConnectHost(host), report.Port))
	}
	report.Healthy = health.QueryReady
	if !report.Healthy {
		return report, ops.failedCleanup(cityPath, report.PID, report.Port, fmt.Errorf("dolt server on %s:%d is not query-ready after recovery", managedDoltConnectHost(host), report.Port))
	}
	if err := ops.publish(cityPath); err != nil {
		return report, ops.failedCleanup(cityPath, report.PID, report.Port, fmt.Errorf("publish managed dolt runtime state: %w", err))
	}
	return report, nil
}

// recoverManagedDoltAdoptRunningServer probes the managed listener and adopts a
// healthy server rather than restarting it.
//
// When queued is true this recovery only acquired the lifecycle lock because
// another gc process released it, so that process's replacement server may
// still be opening its databases. A single failed probe in that window is not
// evidence of a wedged server, and restarting on it is what turns one overload
// into a crashloop: every queued client takes the lock in turn and kills the
// generation its predecessor just started, so none survives long enough to
// finish opening. Re-probe until the caller's deadline instead. A listener that
// stops answering entirely ends the wait immediately — there is no replacement
// to wait for, so the restart below is the right response.
//
// A read-only server is never a warm-up state; it is diagnosed and restarted on
// the first probe exactly as a first-arrival recovery would.
func recoverManagedDoltAdoptRunningServer(
	cityPath, host, port, user string,
	deadline time.Time,
	queued bool,
	ops managedDoltRecoveryOps,
	report *managedDoltRecoverReport,
) (bool, error) {
	for {
		if err := ops.queryProbe(host, port, user); err == nil {
			health, healthErr := ops.healthCheck(host, port, user)
			switch {
			case healthErr == nil && health.ReadOnly == "true":
				report.DiagnosedReadOnly = true
				return false, nil
			case healthErr == nil && health.QueryReady && health.ReadOnly == "false":
				report.Ready = true
				report.Healthy = true
				if err := recoverManagedDoltRepairRuntimeStateForHealthyPort(cityPath, port); err != nil {
					return false, err
				}
				if err := ops.publish(cityPath); err != nil {
					return false, fmt.Errorf("publish managed dolt runtime state: %w", err)
				}
				recoverManagedDoltPopulateReportFromRuntimeState(cityPath, port, report, pidAlive)
				return true, nil
			}
		}
		if !queued || !managedDoltRecoverListenerReachableFn(host, port) || !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(managedDoltRecoverSettleInterval)
	}
}

func cleanupFailedManagedDoltRecovery(cityPath string, pid, port int, cause error) error {
	if cause == nil {
		return nil
	}
	cleanupErrs := make([]error, 0, 3)
	if pid > 0 {
		if err := terminateManagedDoltPID(cityPath, pid); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("cleanup failed: %w", err))
		}
	}
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		cleanupErrs = append(cleanupErrs, err)
	} else {
		portText := ""
		if port > 0 {
			portText = strconv.Itoa(port)
		}
		if err := clearManagedDoltRuntime(layout, portText); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if err := clearManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
		cleanupErrs = append(cleanupErrs, err)
	}
	if len(cleanupErrs) == 0 {
		return cause
	}
	joined := append([]error{cause}, cleanupErrs...)
	return errors.Join(joined...)
}

func managedDoltRecoverFields(report managedDoltRecoverReport) []string {
	return []string{
		"diagnosed_read_only\t" + strconv.FormatBool(report.DiagnosedReadOnly),
		"had_pid\t" + strconv.FormatBool(report.HadPID),
		"forced\t" + strconv.FormatBool(report.Forced),
		"ready\t" + strconv.FormatBool(report.Ready),
		"pid\t" + strconv.Itoa(report.PID),
		"port\t" + strconv.Itoa(report.Port),
		"healthy\t" + strconv.FormatBool(report.Healthy),
		"restarted\t" + strconv.FormatBool(report.Restarted),
	}
}

func recoverManagedDoltExistingObserveTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 5 * time.Second
	}
	if timeout < 5*time.Second {
		return timeout
	}
	return 5 * time.Second
}

func recoverManagedDoltShouldReuseExisting(existingPort int, requestedPort string) bool {
	if existingPort <= 0 {
		return false
	}
	requestedPort = strings.TrimSpace(requestedPort)
	if requestedPort == "" {
		return true
	}
	return strconv.Itoa(existingPort) != requestedPort
}

// recoverManagedDoltObservedRebindPossible reports whether recorded runtime
// state shows a managed server bound somewhere other than the requested port.
//
// alive verifies the recorded pid before the state is trusted; a nil alive uses
// the real process check. Runtime state routinely outlives the process it
// describes — during the ga-oigp outage dolt-state.json advertised a pid that
// had been dead for minutes as running:true — and a dead pid is no evidence
// that some other generation rebound a different port. Believing it sent one
// recovery racer at a port nobody was serving.
func recoverManagedDoltObservedRebindPossible(cityPath, requestedPort string, alive func(int) bool) bool {
	requestedPort = strings.TrimSpace(requestedPort)
	if requestedPort == "" {
		return true
	}
	if alive == nil {
		alive = pidAlive
	}
	for _, path := range []string{providerManagedDoltStatePath(cityPath), managedDoltStatePath(cityPath)} {
		state, err := readDoltRuntimeStateFile(path)
		if err != nil || !state.Running || state.PID <= 0 || state.Port <= 0 {
			continue
		}
		if !alive(state.PID) {
			continue
		}
		if strconv.Itoa(state.Port) != requestedPort {
			return true
		}
	}
	return false
}

func recoverManagedDoltRepairRuntimeStateForHealthyPort(cityPath, requestedPort string) error {
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil || !owned {
		return err
	}
	portNum, err := strconv.Atoi(strings.TrimSpace(requestedPort))
	if err != nil || portNum <= 0 {
		return nil
	}
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return err
	}
	state := doltRuntimeState{
		Running: true,
		Port:    portNum,
		DataDir: layout.DataDir,
	}
	repaired, ok := repairedManagedDoltRuntimeState(cityPath, layout, state)
	if !ok {
		return nil
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, repaired); err != nil {
		return fmt.Errorf("repair provider dolt runtime state: %w", err)
	}
	return nil
}

// recoverManagedDoltPopulateReportFromRuntimeState fills the recovered pid and
// port from recorded runtime state. alive verifies the recorded pid first so a
// stale entry is never reported as the live server; a nil alive uses the real
// process check.
func recoverManagedDoltPopulateReportFromRuntimeState(cityPath, requestedPort string, report *managedDoltRecoverReport, alive func(int) bool) {
	if report == nil {
		return
	}
	if alive == nil {
		alive = pidAlive
	}
	for _, path := range []string{providerManagedDoltStatePath(cityPath), managedDoltStatePath(cityPath)} {
		state, err := readDoltRuntimeStateFile(path)
		if err != nil || !recoverManagedDoltRuntimeStateMatchesRequest(cityPath, requestedPort, state, alive) {
			continue
		}
		report.HadPID = true
		report.PID = state.PID
		report.Port = state.Port
		return
	}
}

func recoverManagedDoltRuntimeStateMatchesRequest(cityPath, requestedPort string, state doltRuntimeState, alive func(int) bool) bool {
	if !state.Running || state.PID <= 0 || state.Port <= 0 {
		return false
	}
	if alive == nil {
		alive = pidAlive
	}
	if !alive(state.PID) {
		return false
	}
	requestedPort = strings.TrimSpace(requestedPort)
	if requestedPort != "" && strconv.Itoa(state.Port) != requestedPort {
		return false
	}
	if dataDir := strings.TrimSpace(state.DataDir); dataDir != "" && !samePath(dataDir, filepath.Join(cityPath, ".beads", "dolt")) {
		return false
	}
	return true
}

func waitForManagedDoltLifecycleOrReady(cityPath, host, port, user string, timeout time.Duration, lockFile *os.File, _ managedDoltRuntimeLayout, report *managedDoltRecoverReport) (bool, bool, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if report != nil {
			if ready := observeExistingManagedDoltForRecovery(cityPath, host, port, user, time.Second, report); ready {
				return true, false, nil
			}
		}
		locked, err := tryManagedDoltLifecycleLock(lockFile)
		if err != nil {
			return false, false, err
		}
		if locked {
			return false, true, nil
		}
		if time.Now().After(deadline) {
			return false, false, fmt.Errorf("timed out waiting for concurrent managed dolt lifecycle to finish")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func observeExistingManagedDoltForRecovery(cityPath, host, port, user string, timeout time.Duration, report *managedDoltRecoverReport) bool {
	existing, err := assessExistingManagedDolt(cityPath, host, port, user, timeout)
	if err != nil {
		return false
	}
	if report != nil {
		if existing.ManagedPID > 0 {
			report.HadPID = true
			report.PID = existing.ManagedPID
		}
		if existing.StatePort > 0 {
			report.Port = existing.StatePort
		}
	}
	if !existing.Reusable || existing.StatePort <= 0 {
		return false
	}
	if report != nil {
		report.Ready = true
		report.Healthy = true
	}
	return true
}
