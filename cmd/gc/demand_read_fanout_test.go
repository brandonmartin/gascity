package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// ga-nj0me: the demand fingerprint and the default scale_check groups each
// read every physical store, and each read is independent of the others. They
// used to run one store at a time, so a tick paid the SUM of every store's
// latency at both read points. These tests pin the fan-out (reads overlap, at
// most probeConcurrency at once, each store read once) and that the fold is
// still the serial one: the same fingerprint bytes, the same error order, the
// same first-leg-wins dedup, whatever order the reads finish in.

// readyAssignedWorkAssignees is the assigned census's assignee enumeration
// with its closed named-session index read, as one call.
func readyAssignedWorkAssignees(cfg *config.City, cityStore beads.Store, sessionBeads *sessionBeadSnapshot, pass *demandPassTrace, point string, reads demandReads) []string {
	closedIdx := readyAssigneesClosedNamedIndex(cfg, cityStore, pass, point, reads)
	return readyAssignedWorkAssigneesFromIndex(cfg, sessionBeads, nil, closedIdx)
}

// fanoutGate lets a test require that want reads be in flight at the same
// time. A pass that reads serially never gets there, so every read waits out
// the timeout and the gate records it.
type fanoutGate struct {
	mu        sync.Mutex
	want      int
	inFlight  int
	maxFlight int
	arrived   int
	timedOut  bool
	timeout   time.Duration
	released  chan struct{}
}

func newFanoutGate(want int) *fanoutGate {
	return &fanoutGate{want: want, timeout: 2 * time.Second, released: make(chan struct{})}
}

// enter blocks until want reads have arrived (or the timeout passes), then
// lets the caller read. leave must follow.
func (g *fanoutGate) enter() {
	g.mu.Lock()
	g.inFlight++
	g.arrived++
	if g.inFlight > g.maxFlight {
		g.maxFlight = g.inFlight
	}
	if g.arrived == g.want {
		close(g.released)
	}
	g.mu.Unlock()
	select {
	case <-g.released:
	case <-time.After(g.timeout):
		g.mu.Lock()
		g.timedOut = true
		g.mu.Unlock()
	}
}

func (g *fanoutGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight--
}

func (g *fanoutGate) check(t *testing.T, limit int) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timedOut {
		t.Fatalf("reads never had %d in flight at once (max %d): the pass reads its stores one at a time", g.want, g.maxFlight)
	}
	if g.maxFlight > limit {
		t.Fatalf("max reads in flight = %d, want at most probe_concurrency %d", g.maxFlight, limit)
	}
}

// scriptedReadyStore answers Ready from a fixed script, optionally through a
// fanoutGate or after another store has answered, and counts its calls.
type scriptedReadyStore struct {
	beads.Store
	rows []beads.Bead
	err  error
	gate *fanoutGate
	// after, when set, holds the answer until that store has answered: a
	// chain of them fixes the order reads finish in.
	after *scriptedReadyStore

	mu       sync.Mutex
	calls    int
	done     chan struct{}
	doneOnce sync.Once
	outOfOrd bool
}

func newScriptedReadyStore(rows []beads.Bead, err error) *scriptedReadyStore {
	return &scriptedReadyStore{Store: beads.NewMemStore(), rows: rows, err: err, done: make(chan struct{})}
}

func (s *scriptedReadyStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.gate != nil {
		s.gate.enter()
		defer s.gate.leave()
	}
	if s.after != nil {
		select {
		case <-s.after.done:
		case <-time.After(2 * time.Second):
			s.mu.Lock()
			s.outOfOrd = true
			s.mu.Unlock()
		}
	}
	defer s.doneOnce.Do(func() { close(s.done) })
	return append([]beads.Bead(nil), s.rows...), s.err
}

// finishInReverse chains stores so each answers only after the next one has:
// the last store answers first and the first answers last. Every read must be
// in flight at once for the chain to complete in order.
func finishInReverse(stores []*scriptedReadyStore) {
	for i := 0; i+1 < len(stores); i++ {
		if stores[i] != stores[i+1] {
			stores[i].after = stores[i+1]
		}
	}
}

// checkChainHeld fails when a store answered before the store it waits on.
func checkChainHeld(t *testing.T, stores []*scriptedReadyStore) {
	t.Helper()
	for i, s := range stores {
		s.mu.Lock()
		broken := s.outOfOrd
		s.mu.Unlock()
		if broken {
			t.Fatalf("store %d answered before its successor: the reads did not overlap", i)
		}
	}
}

func (s *scriptedReadyStore) readyCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func fanoutBead(id, routedTo string) beads.Bead {
	return beads.Bead{
		ID:        id,
		Title:     "title " + id,
		Status:    "open",
		Type:      "task",
		UpdatedAt: time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC),
		Metadata:  map[string]string{beadmeta.RoutedToMetadataKey: routedTo},
	}
}

// tenStoreFingerprintRuntime is a city with a work store and nine serving
// rigs: ten physical stores, the shape of the incident city.
func tenStoreFingerprintRuntime(t *testing.T, city *scriptedReadyStore, rigs []*scriptedReadyStore) *CityRuntime {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	rigStores := make(map[string]beads.Store, len(rigs))
	for i, store := range rigs {
		name := fmt.Sprintf("rig%02d", i)
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Path: filepath.Join(cityPath, name)})
		rigStores[name] = store
	}
	return &CityRuntime{
		cityName: "test-city",
		cityPath: cityPath,
		cfg:      cfg,
		cs: &controllerState{
			cityName:      "test-city",
			cityBeadStore: city,
			beadStores:    rigStores,
			eventProv:     events.NewFake(),
		},
		stderr: io.Discard,
	}
}

// serialReadyDemandFingerprint is the fingerprint as the serial pass computed
// it before the fan-out: one live Ready per leg, hashed as it returns. The
// concurrent pass must produce the same bytes.
func serialReadyDemandFingerprint(cr *CityRuntime) string {
	h := fnv.New64a()
	cfg := cr.serviceConfigSnapshot()
	rigs := cr.rigBeadStores()
	topo := cr.residencyTopology(servingRigStores(cfg, rigs, buildSuspendedRigPathsForCity(cfg, cr.cityPath)))
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err == nil {
		plan, err = storeref.Narrow(plan, storeref.PlaneRuntime)
	}
	if err != nil {
		_, _ = io.WriteString(h, "refused:")
		_, _ = io.WriteString(h, err.Error())
		return fmt.Sprintf("%x", h.Sum64())
	}
	_, _ = storeref.Walk(plan, func(leg storeref.Leg) (bool, error) {
		_, _ = io.WriteString(h, demandFingerprintRef(cr.cityName, leg.Ref))
		_, _ = io.WriteString(h, "\x00")
		if leg.Store == nil {
			_, _ = io.WriteString(h, "<nil>")
			_, _ = io.WriteString(h, "\x00")
			return false, nil
		}
		ready, err := beads.ReadyLive(leg.Store, beads.ReadyQuery{TierMode: beads.TierBoth})
		if err != nil {
			_, _ = io.WriteString(h, "error:")
			_, _ = io.WriteString(h, err.Error())
			_, _ = io.WriteString(h, "\x00")
			return false, nil
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
		for _, bead := range ready {
			writeReadyDemandFingerprintBead(h, bead)
		}
		return false, nil
	})
	return fmt.Sprintf("%x", h.Sum64())
}

func TestReadyDemandFingerprintReadsTenStoresConcurrently(t *testing.T) {
	limit := config.DefaultProbeConcurrency
	gate := newFanoutGate(limit)
	city := newScriptedReadyStore([]beads.Bead{fanoutBead("tc-1", "worker")}, nil)
	city.gate = gate
	var rigs []*scriptedReadyStore
	for i := 0; i < 9; i++ {
		rig := newScriptedReadyStore([]beads.Bead{fanoutBead(fmt.Sprintf("r%d-1", i), "worker")}, nil)
		rig.gate = gate
		rigs = append(rigs, rig)
	}
	cr := tenStoreFingerprintRuntime(t, city, rigs)

	cr.readyDemandSnapshotFingerprint(nil)

	gate.check(t, limit)
	for i, store := range append([]*scriptedReadyStore{city}, rigs...) {
		if got := store.readyCalls(); got != 1 {
			t.Errorf("store %d Ready calls = %d, want 1", i, got)
		}
	}
}

func TestReadyDemandFingerprintMatchesTheSerialFold(t *testing.T) {
	partialErr := &beads.PartialResultError{Op: "ready", Err: errors.New("one leg dark")}
	city := newScriptedReadyStore([]beads.Bead{fanoutBead("tc-2", "worker"), fanoutBead("tc-1", "worker")}, nil)
	shared := newScriptedReadyStore([]beads.Bead{fanoutBead("sh-1", "worker")}, nil)
	rigs := []*scriptedReadyStore{
		newScriptedReadyStore([]beads.Bead{fanoutBead("r0-1", "worker")}, nil),
		newScriptedReadyStore(nil, errors.New("rig01 store unreachable")),
		newScriptedReadyStore([]beads.Bead{fanoutBead("r2-1", "worker")}, partialErr),
		shared,
		shared, // two rigs on one physical store: a duplicate leg
		newScriptedReadyStore(nil, nil),
		newScriptedReadyStore([]beads.Bead{fanoutBead("r6-2", "worker"), fanoutBead("r6-1", "other")}, nil),
		newScriptedReadyStore([]beads.Bead{fanoutBead("tc-1", "worker")}, nil), // same ID as the city
		newScriptedReadyStore([]beads.Bead{fanoutBead("r8-1", "worker")}, nil),
	}
	cr := tenStoreFingerprintRuntime(t, city, rigs)
	want := serialReadyDemandFingerprint(cr)

	// Finish in reverse leg order, every leg in flight at once: a fold in
	// completion order would hash the last leg first.
	ten := 10
	cr.cfg.Daemon.ProbeConcurrency = &ten
	all := append([]*scriptedReadyStore{city}, rigs...)
	finishInReverse(all)
	got := cr.readyDemandSnapshotFingerprint(nil)
	checkChainHeld(t, all)
	if got != want {
		t.Fatalf("fingerprint = %s, want the serial fold's %s", got, want)
	}
	for _, s := range all {
		s.after = nil
	}
	for i := 0; i < 3; i++ {
		if got := cr.readyDemandSnapshotFingerprint(nil); got != want {
			t.Fatalf("pass %d: fingerprint = %s, want the stable serial fold's %s", i, got, want)
		}
	}

	// A healthy leg turning failed must still move the fingerprint: the fold
	// did not drop the error or the rows.
	rigs[0].err = errors.New("now dark")
	if got := cr.readyDemandSnapshotFingerprint(nil); got == want {
		t.Fatal("fingerprint did not change when a healthy leg failed")
	}
	if got, serial := cr.readyDemandSnapshotFingerprint(nil), serialReadyDemandFingerprint(cr); got != serial {
		t.Fatalf("fingerprint after failure = %s, want the serial fold's %s", got, serial)
	}
}

// panicReadyStore panics on Ready.
type panicReadyStore struct{ beads.Store }

func (panicReadyStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	panic("ready blew up")
}

// recoveredPanic runs fn and returns what it panicked with, or nil.
func recoveredPanic(fn func()) (r any) {
	defer func() { r = recover() }()
	fn()
	return nil
}

// A leg read that panics reaches the caller's goroutine with its own value,
// as it did when the read ran inline, instead of killing the process from a
// worker goroutine. The other legs still finish first.
func TestReadyDemandFingerprintRaisesAPanickingLegOnTheCaller(t *testing.T) {
	city := newScriptedReadyStore([]beads.Bead{fanoutBead("tc-1", "worker")}, nil)
	cr := tenStoreFingerprintRuntime(t, city, nil)
	cr.cs.beadStores = map[string]beads.Store{"rig00": panicReadyStore{Store: beads.NewMemStore()}}
	cr.cfg.Rigs = []config.Rig{{Name: "rig00", Path: filepath.Join(cr.cityPath, "rig00")}}

	got := recoveredPanic(func() { cr.readyDemandSnapshotFingerprint(nil) })

	if got != "ready blew up" {
		t.Fatalf("recovered %v, want the leg's own panic value", got)
	}
	if city.readyCalls() != 1 {
		t.Fatalf("city Ready calls = %d, want 1: the healthy leg still reads", city.readyCalls())
	}
}

// tenGroupTargets is ten default scale_check targets, one per physical store,
// all serving template "pool".
func tenGroupTargets(stores []*scriptedReadyStore) []defaultScaleCheckTarget {
	targets := make([]defaultScaleCheckTarget, 0, len(stores))
	for i, store := range stores {
		key := "city"
		if i > 0 {
			key = fmt.Sprintf("rig:rig%02d", i)
		}
		targets = append(targets, defaultScaleCheckTarget{template: "pool", storeKey: key, store: store})
	}
	return targets
}

func TestDefaultScaleCheckDemandReadsTenGroupsConcurrently(t *testing.T) {
	limit := config.DefaultProbeConcurrency
	gate := newFanoutGate(limit)
	var stores []*scriptedReadyStore
	for i := 0; i < 10; i++ {
		store := newScriptedReadyStore([]beads.Bead{fanoutBead(fmt.Sprintf("g%d-1", i), "pool")}, nil)
		store.gate = gate
		stores = append(stores, store)
	}

	counts, _, partials, errs := defaultScaleCheckCountsAndDemand(&config.City{}, tenGroupTargets(stores), newReadyDemandCache())

	gate.check(t, limit)
	if len(errs) != 0 || len(partials) != 0 {
		t.Fatalf("errs = %v, partials = %v, want none", errs, partials)
	}
	if counts["pool"] != 10 {
		t.Fatalf("counts[pool] = %d, want 10", counts["pool"])
	}
	for i, store := range stores {
		if got := store.readyCalls(); got != 1 {
			t.Errorf("group %d Ready calls = %d, want 1", i, got)
		}
	}
}

func TestDefaultScaleCheckDemandFoldsInGroupOrder(t *testing.T) {
	partialErr := &beads.PartialResultError{Op: "ready", Err: errors.New("one leg dark")}
	build := func() ([]*scriptedReadyStore, []defaultScaleCheckTarget) {
		stores := []*scriptedReadyStore{
			newScriptedReadyStore([]beads.Bead{fanoutBead("dup-1", "pool"), fanoutBead("c-1", "pool")}, nil),
			newScriptedReadyStore(nil, errors.New("rig01 unreachable")),
			newScriptedReadyStore([]beads.Bead{fanoutBead("p-1", "pool")}, partialErr),
			newScriptedReadyStore([]beads.Bead{fanoutBead("dup-1", "pool"), fanoutBead("r3-1", "pool")}, nil),
			newScriptedReadyStore(nil, errors.New("rig04 unreachable")),
			newScriptedReadyStore([]beads.Bead{fanoutBead("r5-1", "other")}, nil),
			newScriptedReadyStore([]beads.Bead{fanoutBead("r6-1", "pool")}, nil),
			newScriptedReadyStore([]beads.Bead{fanoutBead("c-1", "pool")}, nil),
			newScriptedReadyStore(nil, nil),
			newScriptedReadyStore([]beads.Bead{fanoutBead("r9-1", "pool")}, nil),
		}
		return stores, tenGroupTargets(stores)
	}

	// The serial answer: probe_concurrency 1 reads one group at a time.
	one := 1
	_, serialTargets := build()
	wantCounts, wantDemand, wantPartials, wantErrs := defaultScaleCheckCountsAndDemand(
		&config.City{Daemon: config.DaemonConfig{ProbeConcurrency: &one}}, serialTargets, newReadyDemandCache())

	stores, targets := build()
	// Finish in reverse group order, every group in flight at once: a fold in
	// completion order would let a later copy of dup-1 win.
	finishInReverse(stores)
	ten := 10
	counts, demand, partials, errs := defaultScaleCheckCountsAndDemand(
		&config.City{Daemon: config.DaemonConfig{ProbeConcurrency: &ten}}, targets, newReadyDemandCache())
	checkChainHeld(t, stores)

	if !reflect.DeepEqual(counts, wantCounts) {
		t.Errorf("counts = %v, want %v", counts, wantCounts)
	}
	if !reflect.DeepEqual(demand, wantDemand) {
		t.Errorf("demand = %+v, want %+v", demand, wantDemand)
	}
	if !reflect.DeepEqual(partials, wantPartials) {
		t.Errorf("partials = %v, want %v", partials, wantPartials)
	}
	if fmt.Sprint(errs) != fmt.Sprint(wantErrs) {
		t.Errorf("errs = %v, want the serial order %v", errs, wantErrs)
	}

	// And the serial answer is the right one, independently of the fan-out.
	if got := demand["pool"].StoreRefs["dup-1"]; got != "city" {
		t.Errorf("dup-1 store ref = %q, want the first leg %q", got, "city")
	}
	if got, want := demand["pool"].WorkBeadIDs, []string{"dup-1", "c-1", "p-1", "r3-1", "r6-1", "r9-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("WorkBeadIDs = %v, want %v", got, want)
	}
	if !partials["pool"] {
		t.Error("pool is not partial; failed and partial groups must retain it")
	}
	if len(errs) != 3 {
		t.Fatalf("errs = %v, want three (two failed groups and one partial)", errs)
	}
	for i, wantKey := range []string{"rig:rig01", "rig:rig02", "rig:rig04"} {
		if got := errs[i].Error(); !errorNames(got, wantKey) {
			t.Errorf("errs[%d] = %q, want it to name %s", i, got, wantKey)
		}
	}
}

// A group read that panics reaches the caller's goroutine, where the demand
// gather's own recovery fails the pass (TestGatherDemandSurvivesCollectorPanic).
func TestDefaultScaleCheckDemandRaisesAPanickingGroupOnTheCaller(t *testing.T) {
	healthy := newScriptedReadyStore([]beads.Bead{fanoutBead("c-1", "pool")}, nil)
	targets := tenGroupTargets([]*scriptedReadyStore{healthy})
	targets = append(targets, defaultScaleCheckTarget{template: "pool", storeKey: "rig:boom", store: panicReadyStore{Store: beads.NewMemStore()}})

	got := recoveredPanic(func() {
		defaultScaleCheckCountsAndDemand(&config.City{}, targets, newReadyDemandCache())
	})

	if got != "ready blew up" {
		t.Fatalf("recovered %v, want the group's own panic value", got)
	}
	if healthy.readyCalls() != 1 {
		t.Fatalf("healthy group Ready calls = %d, want 1", healthy.readyCalls())
	}
}

func errorNames(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// overlapReads is legacy demand reads whose closed named-session index and
// live open List rendezvous: each waits until the other is in flight.
type overlapReads struct {
	legacyDemandReads
	gate *fanoutGate

	mu         sync.Mutex
	indexReads int
}

func (r *overlapReads) RawOpen(store beads.Store) ([]beads.Bead, error) {
	r.gate.enter()
	defer r.gate.leave()
	return r.legacyDemandReads.RawOpen(store)
}

func (r *overlapReads) ClosedNamedIndex(store beads.Store) (session.ClosedNamedSessionBeadIndex, error) {
	r.mu.Lock()
	r.indexReads++
	r.mu.Unlock()
	r.gate.enter()
	defer r.gate.leave()
	return r.legacyDemandReads.ClosedNamedIndex(store)
}

// The closed named-session index the ready-by-assignee probe needs reads only
// closed history, so it is read alongside the census legs, once, rather than
// after all of them.
func TestAssignedWorkReadsTheClosedNamedIndexAlongsideTheCensus(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "helper"}},
		NamedSessions: []config.NamedSession{{Template: "helper", Mode: "on_demand"}},
	}
	// The city store plus its RawOpen leg: two reads that must overlap.
	reads := &overlapReads{gate: newFanoutGate(2)}

	collectAssignedWorkBeadsWithStores("", cfg, beads.NewMemStore(), nil, nil, nil, newReadyDemandCacheWithReads(reads))

	reads.gate.check(t, 2)
	if reads.indexReads != 1 {
		t.Fatalf("closed named index reads = %d, want 1", reads.indexReads)
	}
}

// A city with no on_demand named session pays no closed-index read at all.
func TestAssignedWorkSkipsTheClosedNamedIndexWithoutOnDemandSessions(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "helper"}},
		NamedSessions: []config.NamedSession{{Template: "helper", Mode: "always"}},
	}
	reads := &overlapReads{gate: newFanoutGate(1)}

	collectAssignedWorkBeadsWithStores("", cfg, beads.NewMemStore(), nil, nil, nil, newReadyDemandCacheWithReads(reads))

	if reads.indexReads != 0 {
		t.Fatalf("closed named index reads = %d, want 0", reads.indexReads)
	}
}

// panicIndexReads is legacy demand reads whose closed named-session index
// panics.
type panicIndexReads struct{ legacyDemandReads }

func (panicIndexReads) ClosedNamedIndex(beads.Store) (session.ClosedNamedSessionBeadIndex, error) {
	panic("index blew up")
}

// The closed-index read moved off the caller's goroutine; its panic still
// reaches the caller, as it did when the read ran inline.
func TestAssignedWorkRaisesAClosedNamedIndexPanicOnTheCaller(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "helper"}},
		NamedSessions: []config.NamedSession{{Template: "helper", Mode: "on_demand"}},
	}

	got := recoveredPanic(func() {
		collectAssignedWorkBeadsWithStores("", cfg, beads.NewMemStore(), nil, nil, nil, newReadyDemandCacheWithReads(panicIndexReads{}))
	})

	if got != "index blew up" {
		t.Fatalf("recovered %v, want the index read's own panic value", got)
	}
}
