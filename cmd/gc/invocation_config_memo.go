package main

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

// One city config composition per one-shot invocation.
//
// A one-shot command reaches the composed city config from many places that
// cannot share a *config.City: the command's own load, the store open (the
// store config and the pinned bd binary), the bd environment builder, the
// hosted-credential probe, the storage-routing funnel, and the mail provider
// lookup, which alone asks three times. Each composed city.toml and every pack
// again. Measured on upstream main against a 9-rig city with 16 packs, `gc mail
// inbox` composed it 16 times, `gc session list` 11 and `gc bd list` 6, and
// composition was most of each command's CPU (ga-ta1h.1).
//
// While the memo is active, loadCityConfigMemoized composes each (city.toml,
// load options) pair once and hands every later caller the same *config.City
// and Provenance. It sits under the loader wrappers, so each wrapper still runs
// its own per-call side effects (warnings, pack runtime validation, feature
// flags) on the shared result. All the hot callers load with the same options
// ({SkipRevisionSnapshot: true}), so they share one entry.
//
// The result is shared, not copied, so callers must not mutate it. The only
// cmd/gc code that mutates a loaded config is the config writers, and they
// load through config.Parse or loadCityConfigForEditFS, which never reach the
// memo.
//
// The memo is opt-out per command. run() activates it for every command except
// the long-lived entry points in invocationConfigMemoLongLivedCommands, which
// reload config on purpose. The processes those start (the supervisor, the
// controller and the API server they host, the nudge poller) also leave the
// memo explicitly before their loops (leaveInvocationConfigMemo), so a loop
// reached some other way still composes every reload.
//
// An entry is dropped, so the next load composes again, when:
//   - a writer changes what the load reads: every config write helper calls
//     invalidateInvocationConfigMemo (writeCityConfigForEditFS,
//     writeCityPackManifest, gc rig remove, gc pack's lock write, doctor's
//     site-binding repair), and so does validateComposedConfigAfterInstall
//     after `gc import install` materializes packs;
//   - the builtin-pack refresh runs for the first time in this invocation
//     (noteBuiltinPackRefresh): an entry composed before it may predate files
//     the refresh rewrote;
//   - any file the load reported as a source, packs.lock or .gc/site.toml no
//     longer has the size and mtime it had when the load ran, which catches a
//     rewrite by a writer that does not call the hook.
//
// Errors are never memoized, and only real-filesystem loads are.

// invocationConfigMemoLongLivedCommands are the command paths run() never
// activates the memo for: each runs a loop that reloads city config.
var invocationConfigMemoLongLivedCommands = map[string]bool{
	"gc dashboard serve": true,
	"gc nudge poll":      true,
	"gc start":           true,
	"gc supervisor run":  true,
}

// invocationConfigMemoEligible reports whether run() activates the memo for cmd.
func invocationConfigMemoEligible(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	return !invocationConfigMemoLongLivedCommands[cmd.CommandPath()]
}

type invocationConfigKey struct {
	tomlPath string
	opts     config.LoadOptions
}

type invocationConfigEntry struct {
	cfg     *config.City
	prov    *config.Provenance
	sources []invocationConfigSource
}

// invocationConfigSource is one file a memoized load depends on, as it was
// when the load ran. A file that did not exist is recorded too, so creating
// it drops the entry.
type invocationConfigSource struct {
	path    string
	exists  bool
	size    int64
	modTime time.Time
}

// invocationConfigMemo is the memo of one invocation. A nil memo is disabled.
type invocationConfigMemo struct {
	mu      sync.Mutex
	entries map[invocationConfigKey]*invocationConfigEntry
	// refreshed holds the city.toml paths whose builtin-pack refresh has run
	// in this invocation.
	refreshed map[string]bool
}

var (
	invocationConfigsMu sync.Mutex
	invocationConfigs   *invocationConfigMemo
)

// invocationConfigLoads counts every composition loadCityConfigMemoized
// performs, memo active or not, so tests can assert how many compositions an
// invocation made.
var invocationConfigLoads atomic.Int64

// beginInvocationConfigMemo activates the memo for the rest of this invocation
// and returns the function that ends it. Calling the end function more than
// once is safe.
func beginInvocationConfigMemo() (end func()) {
	memo := &invocationConfigMemo{
		entries:   make(map[invocationConfigKey]*invocationConfigEntry),
		refreshed: make(map[string]bool),
	}
	invocationConfigsMu.Lock()
	previous := invocationConfigs
	invocationConfigs = memo
	invocationConfigsMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			invocationConfigsMu.Lock()
			if invocationConfigs == memo {
				invocationConfigs = previous
			}
			invocationConfigsMu.Unlock()
		})
	}
}

// leaveInvocationConfigMemo deactivates the memo for the rest of the process.
// A long-lived host calls it before its loop starts, so every reload composes
// the config from disk.
func leaveInvocationConfigMemo() {
	invocationConfigsMu.Lock()
	invocationConfigs = nil
	invocationConfigsMu.Unlock()
}

// activeInvocationConfigMemo returns the active memo, or nil.
func activeInvocationConfigMemo() *invocationConfigMemo {
	invocationConfigsMu.Lock()
	defer invocationConfigsMu.Unlock()
	return invocationConfigs
}

// invalidateInvocationConfigMemo drops every entry of the active memo. Config
// writers call it so the next load in the same invocation reads what they
// wrote.
func invalidateInvocationConfigMemo() {
	memo := activeInvocationConfigMemo()
	if memo == nil {
		return
	}
	memo.mu.Lock()
	clear(memo.entries)
	memo.mu.Unlock()
}

// noteBuiltinPackRefresh records that the builtin-pack refresh for tomlPath's
// city has run. The first time it runs in an invocation it drops that city's
// entries: they were composed before the refresh and may predate files it
// rewrote. Later refreshes in the same invocation find the packs ready and
// keep them.
func noteBuiltinPackRefresh(tomlPath string) {
	memo := activeInvocationConfigMemo()
	if memo == nil {
		return
	}
	path := invocationConfigPath(tomlPath)
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if memo.refreshed[path] {
		return
	}
	memo.refreshed[path] = true
	for key := range memo.entries {
		if key.tomlPath == path {
			delete(memo.entries, key)
		}
	}
}

// loadCityConfigMemoized is config.LoadWithIncludesOptions behind the
// invocation's memo. The returned config and Provenance may be shared with
// other callers in this invocation and must not be mutated.
func loadCityConfigMemoized(fs fsys.FS, tomlPath string, opts config.LoadOptions) (*config.City, *config.Provenance, error) {
	memo := activeInvocationConfigMemo()
	if memo == nil || !usesOSFS(fs) {
		invocationConfigLoads.Add(1)
		return config.LoadWithIncludesOptions(fs, tomlPath, opts)
	}
	key := invocationConfigKey{tomlPath: invocationConfigPath(tomlPath), opts: opts}

	memo.mu.Lock()
	entry := memo.entries[key]
	memo.mu.Unlock()
	if entry != nil && entry.current() {
		return entry.cfg, entry.prov, nil
	}

	before := statInvocationConfigSource(tomlPath)
	invocationConfigLoads.Add(1)
	cfg, prov, err := config.LoadWithIncludesOptions(fs, tomlPath, opts)
	if err != nil {
		return nil, nil, err
	}
	// A city.toml rewritten while the load ran could pair the new file's stat
	// with the old file's content; leave such a load unmemoized.
	if before.unchanged() {
		entry = &invocationConfigEntry{cfg: cfg, prov: prov, sources: invocationConfigSources(tomlPath, prov)}
		memo.mu.Lock()
		memo.entries[key] = entry
		memo.mu.Unlock()
	}
	return cfg, prov, nil
}

func invocationConfigPath(tomlPath string) string {
	if abs, err := filepath.Abs(tomlPath); err == nil {
		tomlPath = abs
	}
	return normalizePathForCompare(tomlPath)
}

// invocationConfigSources lists the files a load depends on: every source the
// load reported (city.toml, the city pack.toml, includes, implicit imports),
// plus the lock file that pins imported packs and the machine-local site
// bindings, neither of which the load reports as a source.
func invocationConfigSources(tomlPath string, prov *config.Provenance) []invocationConfigSource {
	cityDir := filepath.Dir(tomlPath)
	paths := []string{tomlPath, filepath.Join(cityDir, "packs.lock"), filepath.Join(cityDir, ".gc", "site.toml")}
	if prov != nil {
		paths = append(paths, prov.Sources...)
	}
	seen := make(map[string]struct{}, len(paths))
	sources := make([]invocationConfigSource, 0, len(paths))
	for _, path := range paths {
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		sources = append(sources, statInvocationConfigSource(path))
	}
	return sources
}

func statInvocationConfigSource(path string) invocationConfigSource {
	fi, err := os.Stat(path)
	if err != nil {
		return invocationConfigSource{path: path}
	}
	return invocationConfigSource{path: path, exists: true, size: fi.Size(), modTime: fi.ModTime()}
}

func (s invocationConfigSource) unchanged() bool {
	now := statInvocationConfigSource(s.path)
	return now.exists == s.exists && now.size == s.size && now.modTime.Equal(s.modTime)
}

// current reports whether every file the entry's load depended on is as it
// was when the load ran.
func (e *invocationConfigEntry) current() bool {
	for _, source := range e.sources {
		if !source.unchanged() {
			return false
		}
	}
	return true
}
