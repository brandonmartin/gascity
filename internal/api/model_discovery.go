package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	workerbuiltin "github.com/gastownhall/gascity/internal/worker/builtin"
)

// Model discovery cache policy (ga-k67).
//
// A listing is re-run at most once per (binary path, size, mtime) per
// positive TTL, and a failed listing (logged-out CLI, wrong verb, timeout) is
// not retried before the negative TTL. The in-memory layer keeps a
// long-running server from touching disk on every picker read while still
// noticing a refresh made by another process within a minute.
const (
	modelDiscoveryTimeout     = 15 * time.Second
	modelDiscoveryMemoryTTL   = time.Minute
	modelDiscoveryPositiveTTL = 24 * time.Hour
	modelDiscoveryNegativeTTL = 10 * time.Minute
	modelDiscoveryCacheDir    = "gascity/model-discovery"
)

// modelListingRunner is the narrow executor seam: run one listing verb and
// return its stdout. The production runner is the readiness probes' exec
// path (runProbeCommandWithEnv over providerProbeCommandContext); tests
// script it.
type modelListingRunner func(ctx context.Context, binary string, args, passthroughEnv []string, timeout time.Duration) (string, error)

// ProviderModelDiscovery runs a provider binary's declared listing verb and
// caches the ids it reports, persistently per binary version under the user
// cache dir and in memory for the life of the process. It is the process's
// config.ModelDiscoverer, installed once at the gc entry point; test
// processes never install one, so unit tests see the curated catalog only.
//
// Every failure is soft: an absent binary, an exec error, a timeout, or
// output the parser cannot read all yield nil, which callers merge as
// "nothing discovered" — the curated seed stays intact.
type ProviderModelDiscovery struct {
	cacheDir string
	lookPath func(string) (string, error)
	stat     func(string) (os.FileInfo, error)
	run      modelListingRunner
	now      func() time.Time
	logf     func(format string, args ...any)

	mu     sync.Mutex
	memory map[string]memoryModelDiscovery
}

var _ config.ModelDiscoverer = (*ProviderModelDiscovery)(nil)

type memoryModelDiscovery struct {
	entry    modelDiscoveryCacheEntry
	cachedAt time.Time
}

// modelDiscoveryCacheEntry is the on-disk record for one (binary, listing)
// pair. Binary identity fields must match the current binary for the entry
// to be honored; DiscoveredAt plus the OK-dependent TTL bounds its life.
type modelDiscoveryCacheEntry struct {
	Binary       string                 `json:"binary"`
	Size         int64                  `json:"size"`
	ModTimeNanos int64                  `json:"mtime_unix_nano"`
	Args         []string               `json:"args"`
	Format       string                 `json:"format"`
	Prefix       string                 `json:"prefix,omitempty"`
	DiscoveredAt time.Time              `json:"discovered_at"`
	OK           bool                   `json:"ok"`
	Error        string                 `json:"error,omitempty"`
	Models       []discoveredModelEntry `json:"models,omitempty"`
}

type discoveredModelEntry struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// modelDiscoverySeams are the Layer-0 dependencies of a discoverer: binary
// lookup, binary identity, the listing executor, the clock, and the log.
// Production wires the real ones in NewProviderModelDiscovery; tests build
// the discoverer with scripted seams so no test body reaches os/exec.
type modelDiscoverySeams struct {
	lookPath func(string) (string, error)
	stat     func(string) (os.FileInfo, error)
	run      modelListingRunner
	now      func() time.Time
	logf     func(format string, args ...any)
}

// NewProviderModelDiscovery returns a discoverer persisting under cacheDir.
// An empty cacheDir disables persistence (in-memory memoization only).
func NewProviderModelDiscovery(cacheDir string) *ProviderModelDiscovery {
	return newProviderModelDiscovery(cacheDir, modelDiscoverySeams{
		lookPath: exec.LookPath,
		stat:     os.Stat,
		run:      runProviderModelListing,
		now:      time.Now,
		logf:     log.Printf,
	})
}

func newProviderModelDiscovery(cacheDir string, seams modelDiscoverySeams) *ProviderModelDiscovery {
	return &ProviderModelDiscovery{
		cacheDir: cacheDir,
		lookPath: seams.lookPath,
		stat:     seams.stat,
		run:      seams.run,
		now:      seams.now,
		logf:     seams.logf,
		memory:   make(map[string]memoryModelDiscovery),
	}
}

// DefaultProviderModelDiscovery returns the process-wide discoverer, caching
// under the user cache dir (XDG_CACHE_HOME or ~/.cache). The cache is keyed
// by binary identity, not by city: one cursor-agent serves every city on the
// host, so a per-city cache would only multiply the cold execs.
func DefaultProviderModelDiscovery() *ProviderModelDiscovery {
	cacheDir := ""
	if base, err := os.UserCacheDir(); err == nil && base != "" {
		cacheDir = filepath.Join(base, filepath.FromSlash(modelDiscoveryCacheDir))
	}
	return NewProviderModelDiscovery(cacheDir)
}

// DiscoverModels implements config.ModelDiscoverer.
func (d *ProviderModelDiscovery) DiscoverModels(ctx context.Context, req config.ModelDiscoveryRequest) []config.DiscoveredModel {
	binary, err := d.lookPath(req.Binary)
	if err != nil || binary == "" {
		// Not installed on this PATH: the session could not launch it
		// either, so there is nothing to discover and nothing to report.
		return nil
	}
	info, err := d.stat(binary)
	if err != nil {
		return nil
	}
	identity := modelDiscoveryCacheEntry{
		Binary:       binary,
		Size:         info.Size(),
		ModTimeNanos: info.ModTime().UnixNano(),
		Args:         append([]string(nil), req.Discovery.Args...),
		Format:       req.Discovery.Format,
		Prefix:       req.Discovery.Prefix,
	}
	key := modelDiscoveryCacheKey(identity)
	now := d.now()

	if !req.Fresh {
		if entry, ok := d.loadMemory(key, identity, now); ok {
			return entry.models()
		}
		if entry, ok := d.loadFile(key, identity, now); ok {
			d.storeMemory(key, entry, now)
			return entry.models()
		}
	}

	entry := d.execute(ctx, identity, req, now)
	d.storeMemory(key, entry, now)
	d.storeFile(key, entry)
	return entry.models()
}

func (d *ProviderModelDiscovery) execute(ctx context.Context, identity modelDiscoveryCacheEntry, req config.ModelDiscoveryRequest, now time.Time) modelDiscoveryCacheEntry {
	entry := identity
	entry.DiscoveredAt = now

	stdout, err := d.run(ctx, identity.Binary, identity.Args, req.PassthroughEnv, modelDiscoveryTimeout)
	if err == nil {
		var models []workerbuiltin.DiscoveredModel
		models, err = workerbuiltin.ParseModelListing(identity.Format, stdout)
		if err == nil {
			models = filterModelPrefix(models, identity.Prefix)
			if len(models) == 0 {
				err = fmt.Errorf("%w with prefix %q", workerbuiltin.ErrNoModelsParsed, identity.Prefix)
			}
		}
		for _, model := range models {
			entry.Models = append(entry.Models, discoveredModelEntry{ID: model.ID, Label: model.Label})
		}
	}
	if err != nil {
		entry.Error = err.Error()
		d.logf("model discovery: provider %s: %s %s: %v (using curated model list for %s)",
			req.Provider, identity.Binary, strings.Join(identity.Args, " "), err, modelDiscoveryNegativeTTL)
		return entry
	}
	entry.OK = true
	return entry
}

func filterModelPrefix(models []workerbuiltin.DiscoveredModel, prefix string) []workerbuiltin.DiscoveredModel {
	if prefix == "" {
		return models
	}
	out := models[:0:0]
	for _, model := range models {
		if strings.HasPrefix(model.ID, prefix) {
			out = append(out, model)
		}
	}
	return out
}

func (e modelDiscoveryCacheEntry) models() []config.DiscoveredModel {
	if !e.OK || len(e.Models) == 0 {
		return nil
	}
	out := make([]config.DiscoveredModel, len(e.Models))
	for i, model := range e.Models {
		out[i] = config.DiscoveredModel{ID: model.ID, Label: model.Label}
	}
	return out
}

func (e modelDiscoveryCacheEntry) sameBinaryAndListing(identity modelDiscoveryCacheEntry) bool {
	if e.Binary != identity.Binary || e.Size != identity.Size || e.ModTimeNanos != identity.ModTimeNanos {
		return false
	}
	if e.Format != identity.Format || e.Prefix != identity.Prefix || len(e.Args) != len(identity.Args) {
		return false
	}
	for i := range e.Args {
		if e.Args[i] != identity.Args[i] {
			return false
		}
	}
	return true
}

func (e modelDiscoveryCacheEntry) validAt(now time.Time) bool {
	ttl := modelDiscoveryNegativeTTL
	if e.OK {
		ttl = modelDiscoveryPositiveTTL
	}
	return !e.DiscoveredAt.IsZero() && now.Before(e.DiscoveredAt.Add(ttl))
}

func (d *ProviderModelDiscovery) loadMemory(key string, identity modelDiscoveryCacheEntry, now time.Time) (modelDiscoveryCacheEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cached, ok := d.memory[key]
	if !ok {
		return modelDiscoveryCacheEntry{}, false
	}
	if now.After(cached.cachedAt.Add(modelDiscoveryMemoryTTL)) || !cached.entry.sameBinaryAndListing(identity) || !cached.entry.validAt(now) {
		delete(d.memory, key)
		return modelDiscoveryCacheEntry{}, false
	}
	return cached.entry, true
}

func (d *ProviderModelDiscovery) storeMemory(key string, entry modelDiscoveryCacheEntry, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.memory[key] = memoryModelDiscovery{entry: entry, cachedAt: now}
}

func (d *ProviderModelDiscovery) cachePath(key string) string {
	if d.cacheDir == "" {
		return ""
	}
	return filepath.Join(d.cacheDir, key+".json")
}

func (d *ProviderModelDiscovery) loadFile(key string, identity modelDiscoveryCacheEntry, now time.Time) (modelDiscoveryCacheEntry, bool) {
	path := d.cachePath(key)
	if path == "" {
		return modelDiscoveryCacheEntry{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return modelDiscoveryCacheEntry{}, false
	}
	var entry modelDiscoveryCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		// A corrupt entry is a miss; the next successful run overwrites it.
		return modelDiscoveryCacheEntry{}, false
	}
	if !entry.sameBinaryAndListing(identity) || !entry.validAt(now) {
		return modelDiscoveryCacheEntry{}, false
	}
	return entry, true
}

func (d *ProviderModelDiscovery) storeFile(key string, entry modelDiscoveryCacheEntry) {
	path := d.cachePath(key)
	if path == "" {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(d.cacheDir, 0o755); err != nil {
		d.logf("model discovery: cache dir %s: %v", d.cacheDir, err)
		return
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, data, 0o644); err != nil {
		d.logf("model discovery: writing %s: %v", path, err)
	}
}

// modelDiscoveryCacheKey names the cache entry for one (binary, listing)
// pair: a readable binary basename plus a hash of the path and listing
// declaration, so the same binary reached through two paths, or two
// prefixes over one binary, never share an entry.
func modelDiscoveryCacheKey(identity modelDiscoveryCacheEntry) string {
	h := sha256.New()
	h.Write([]byte(identity.Binary))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(identity.Args, "\x00")))
	h.Write([]byte{0})
	h.Write([]byte(identity.Format))
	h.Write([]byte{0})
	h.Write([]byte(identity.Prefix))
	return filepath.Base(identity.Binary) + "-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// runProviderModelListing is the production modelListingRunner. It reuses the
// readiness probes' exec seam — same deterministic env, same test hook, same
// timeout handling — and additionally forwards the process PATH (so the
// binary's own helpers resolve exactly as at session launch) and the
// harness's credential env names (a listing is account-scoped).
func runProviderModelListing(ctx context.Context, binary string, args, passthroughEnv []string, timeout time.Duration) (string, error) {
	homeDir, err := workspaceHomeDir()
	if err != nil {
		return "", errors.New("workspace home unavailable")
	}
	stdout, stderr, err := runProbeCommandWithEnv(ctx, homeDir, timeout, modelListingExtraEnv(passthroughEnv, os.Getenv), binary, args...)
	if err != nil {
		if stderr != "" {
			return "", fmt.Errorf("%w: %s", err, firstLine(stderr))
		}
		return "", err
	}
	return stdout, nil
}

// modelListingExtraEnv builds the env entries layered over probeCommandEnv:
// the named credential variables that are set, then the process PATH. Later
// entries win in exec, so PATH here overrides the probe search path.
func modelListingExtraEnv(passthroughEnv []string, getenv func(string) string) []string {
	var env []string
	for _, key := range passthroughEnv {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			env = append(env, key+"="+value)
		}
	}
	if path := getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	return env
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}
