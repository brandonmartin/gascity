package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	workerbuiltin "github.com/gastownhall/gascity/internal/worker/builtin"
)

// fakeBinaryInfo is the os.FileInfo the discoverer sees for a fake binary.
// Size and ModTime are the binary-identity inputs of the cache key.
type fakeBinaryInfo struct {
	size    int64
	modTime time.Time
}

func (f fakeBinaryInfo) Name() string       { return "cursor-agent" }
func (f fakeBinaryInfo) Size() int64        { return f.size }
func (f fakeBinaryInfo) Mode() os.FileMode  { return 0o755 }
func (f fakeBinaryInfo) ModTime() time.Time { return f.modTime }
func (f fakeBinaryInfo) IsDir() bool        { return false }
func (f fakeBinaryInfo) Sys() any           { return nil }

// scriptedListing is the narrow executor seam with scripted results: it
// records every listing run and answers with a canned stdout or error.
type scriptedListing struct {
	calls  []scriptedListingCall
	stdout string
	err    error
}

type scriptedListingCall struct {
	binary string
	args   []string
	env    []string
}

func (s *scriptedListing) run(_ context.Context, binary string, args, passthroughEnv []string, _ time.Duration) (string, error) {
	s.calls = append(s.calls, scriptedListingCall{binary: binary, args: args, env: passthroughEnv})
	return s.stdout, s.err
}

type discoveryHarness struct {
	d        *ProviderModelDiscovery
	listing  *scriptedListing
	now      time.Time
	binaries map[string]fakeBinaryInfo
	logs     []string
}

func newDiscoveryHarness(t *testing.T, cacheDir string) *discoveryHarness {
	t.Helper()
	h := &discoveryHarness{
		listing: &scriptedListing{stdout: cursorListingFixture},
		now:     time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		binaries: map[string]fakeBinaryInfo{
			"/fake/bin/cursor-agent": {size: 1234, modTime: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)},
			"/fake/bin/opencode":     {size: 99, modTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		},
	}
	h.d = h.newDiscovery(cacheDir)
	return h
}

// newDiscovery builds a discoverer sharing the harness's fakes — a second
// instance over the same cacheDir models a second gc process.
func (h *discoveryHarness) newDiscovery(cacheDir string) *ProviderModelDiscovery {
	return newProviderModelDiscovery(cacheDir, modelDiscoverySeams{
		lookPath: func(name string) (string, error) {
			path := "/fake/bin/" + name
			if _, ok := h.binaries[path]; !ok {
				return "", errors.New("not found")
			}
			return path, nil
		},
		stat: func(path string) (os.FileInfo, error) {
			info, ok := h.binaries[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return info, nil
		},
		run:  h.listing.run,
		now:  func() time.Time { return h.now },
		logf: func(format string, _ ...any) { h.logs = append(h.logs, format) },
	})
}

const cursorListingFixture = "" +
	"Available models\n" +
	"\n" +
	"auto - Auto (default)\n" +
	"claude-opus-5-5-high - Claude Opus 5.5 1M\n"

var cursorRequest = config.ModelDiscoveryRequest{
	Provider:       "cursor",
	Binary:         "cursor-agent",
	Discovery:      config.ModelDiscovery{Args: []string{"--list-models"}, Format: workerbuiltin.ModelListFormatCursor},
	PassthroughEnv: []string{"CURSOR_API_KEY"},
}

var wantCursorModels = []config.DiscoveredModel{
	{ID: "auto", Label: "Auto (default)"},
	{ID: "claude-opus-5-5-high", Label: "Claude Opus 5.5 1M"},
}

func TestProviderModelDiscoveryRunsListingAndCachesInMemory(t *testing.T) {
	h := newDiscoveryHarness(t, t.TempDir())

	got := h.d.DiscoverModels(context.Background(), cursorRequest)
	if !reflect.DeepEqual(got, wantCursorModels) {
		t.Fatalf("models = %+v, want %+v", got, wantCursorModels)
	}
	if len(h.listing.calls) != 1 {
		t.Fatalf("listing ran %d times, want 1", len(h.listing.calls))
	}
	call := h.listing.calls[0]
	if call.binary != "/fake/bin/cursor-agent" || !reflect.DeepEqual(call.args, []string{"--list-models"}) || !reflect.DeepEqual(call.env, []string{"CURSOR_API_KEY"}) {
		t.Fatalf("listing call = %+v", call)
	}

	// Second read within the memory TTL: no exec, no disk.
	again := h.d.DiscoverModels(context.Background(), cursorRequest)
	if !reflect.DeepEqual(again, wantCursorModels) || len(h.listing.calls) != 1 {
		t.Fatalf("cached read: models = %+v, runs = %d", again, len(h.listing.calls))
	}
	if len(h.logs) != 0 {
		t.Fatalf("successful discovery logged: %v", h.logs)
	}
}

func TestProviderModelDiscoveryPersistsAcrossProcesses(t *testing.T) {
	cacheDir := t.TempDir()
	h := newDiscoveryHarness(t, cacheDir)
	if got := h.d.DiscoverModels(context.Background(), cursorRequest); !reflect.DeepEqual(got, wantCursorModels) {
		t.Fatalf("models = %+v", got)
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("cache dir entries = %v, err = %v; want exactly one cache file", entries, err)
	}
	if filepath.Ext(entries[0].Name()) != ".json" {
		t.Fatalf("cache file %q is not JSON", entries[0].Name())
	}

	// A new discoverer over the same cache dir — another gc process, one
	// hour later (past the in-memory TTL) — answers from disk, no exec.
	h.now = h.now.Add(time.Hour)
	second := h.newDiscovery(cacheDir)
	if got := second.DiscoverModels(context.Background(), cursorRequest); !reflect.DeepEqual(got, wantCursorModels) {
		t.Fatalf("models from disk cache = %+v", got)
	}
	if len(h.listing.calls) != 1 {
		t.Fatalf("listing ran %d times across two processes, want 1 (cold once per binary version)", len(h.listing.calls))
	}
}

func TestProviderModelDiscoveryReRunsWhenBinaryChanges(t *testing.T) {
	cacheDir := t.TempDir()
	h := newDiscoveryHarness(t, cacheDir)
	h.d.DiscoverModels(context.Background(), cursorRequest)

	// The binary was upgraded in place: same path, new mtime and size.
	h.binaries["/fake/bin/cursor-agent"] = fakeBinaryInfo{size: 4321, modTime: h.now}
	h.listing.stdout = cursorListingFixture + "claude-fable-5-1-xhigh - Claude Fable 5.1 1M Extra High\n"

	got := h.d.DiscoverModels(context.Background(), cursorRequest)
	if len(h.listing.calls) != 2 {
		t.Fatalf("listing ran %d times, want 2 (once per binary version)", len(h.listing.calls))
	}
	if len(got) != 3 || got[2].ID != "claude-fable-5-1-xhigh" {
		t.Fatalf("models after upgrade = %+v", got)
	}

	// The on-disk entry was replaced, so a fresh process sees the new list.
	if got := h.newDiscovery(cacheDir).DiscoverModels(context.Background(), cursorRequest); len(got) != 3 || len(h.listing.calls) != 2 {
		t.Fatalf("disk cache after upgrade: models = %+v, runs = %d", got, len(h.listing.calls))
	}
}

func TestProviderModelDiscoveryFreshBypassesEveryCache(t *testing.T) {
	h := newDiscoveryHarness(t, t.TempDir())
	h.d.DiscoverModels(context.Background(), cursorRequest)

	fresh := cursorRequest
	fresh.Fresh = true
	h.listing.stdout = "brand-new - Brand New\n"
	got := h.d.DiscoverModels(context.Background(), fresh)
	if len(h.listing.calls) != 2 {
		t.Fatalf("fresh read ran the listing %d times total, want 2", len(h.listing.calls))
	}
	if len(got) != 1 || got[0].ID != "brand-new" {
		t.Fatalf("fresh models = %+v", got)
	}
	// The fresh result replaces the cached one for later non-fresh reads.
	if got := h.d.DiscoverModels(context.Background(), cursorRequest); len(got) != 1 || got[0].ID != "brand-new" || len(h.listing.calls) != 2 {
		t.Fatalf("post-fresh cached read = %+v, runs = %d", got, len(h.listing.calls))
	}
}

func TestProviderModelDiscoveryPositiveEntriesExpire(t *testing.T) {
	// Listings are served by the vendor, so new ids appear without a binary
	// upgrade: a positive entry is re-run once a day even for the same binary.
	h := newDiscoveryHarness(t, t.TempDir())
	h.d.DiscoverModels(context.Background(), cursorRequest)

	h.now = h.now.Add(23 * time.Hour)
	h.d.DiscoverModels(context.Background(), cursorRequest)
	if len(h.listing.calls) != 1 {
		t.Fatalf("listing re-ran inside the positive TTL: %d runs", len(h.listing.calls))
	}
	h.now = h.now.Add(2 * time.Hour)
	h.d.DiscoverModels(context.Background(), cursorRequest)
	if len(h.listing.calls) != 2 {
		t.Fatalf("listing did not re-run after the positive TTL: %d runs", len(h.listing.calls))
	}
}

func TestProviderModelDiscoveryFailuresAreSoftAndNegativelyCached(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		err    error
	}{
		{name: "exec error", err: errors.New("exit status 1: Authentication required")},
		{name: "unparseable output", stdout: "Error: Authentication required. Run 'agent login'.\n"},
		{name: "empty output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			h := newDiscoveryHarness(t, cacheDir)
			h.listing.stdout, h.listing.err = tc.stdout, tc.err

			if got := h.d.DiscoverModels(context.Background(), cursorRequest); got != nil {
				t.Fatalf("failed discovery returned %+v, want nil", got)
			}
			if len(h.logs) != 1 {
				t.Fatalf("failure logged %d times, want once: %v", len(h.logs), h.logs)
			}

			// Retrying immediately — from this process or a new one — does
			// not hammer the binary: the failure is cached.
			h.d.DiscoverModels(context.Background(), cursorRequest)
			h.newDiscovery(cacheDir).DiscoverModels(context.Background(), cursorRequest)
			if len(h.listing.calls) != 1 {
				t.Fatalf("listing ran %d times after a failure, want 1 (negative cache)", len(h.listing.calls))
			}

			// After the negative TTL the binary gets another chance, and a
			// login in the meantime turns the entry positive.
			h.now = h.now.Add(11 * time.Minute)
			h.listing.stdout, h.listing.err = cursorListingFixture, nil
			if got := h.d.DiscoverModels(context.Background(), cursorRequest); !reflect.DeepEqual(got, wantCursorModels) || len(h.listing.calls) != 2 {
				t.Fatalf("recovery: models = %+v, runs = %d", got, len(h.listing.calls))
			}
		})
	}
}

func TestProviderModelDiscoveryAbsentBinaryRunsNothing(t *testing.T) {
	cacheDir := t.TempDir()
	h := newDiscoveryHarness(t, cacheDir)

	req := cursorRequest
	req.Binary = "grok"
	if got := h.d.DiscoverModels(context.Background(), req); got != nil {
		t.Fatalf("absent binary returned %+v", got)
	}
	if len(h.listing.calls) != 0 || len(h.logs) != 0 {
		t.Fatalf("absent binary: runs = %d, logs = %v; want none (not an error, just uninstalled)", len(h.listing.calls), h.logs)
	}
	if entries, _ := os.ReadDir(cacheDir); len(entries) != 0 {
		t.Fatalf("absent binary wrote cache entries: %v", entries)
	}
}

func TestProviderModelDiscoveryPrefixScopesSharedBinaries(t *testing.T) {
	h := newDiscoveryHarness(t, t.TempDir())
	h.listing.stdout = "" +
		"cerebras/gpt-oss-120b\n" +
		"groq/openai/gpt-oss-120b\n" +
		"groq/llama-3.3-70b-versatile\n" +
		"opencode/big-pickle\n"

	groq := config.ModelDiscoveryRequest{
		Provider:  "groq",
		Binary:    "opencode",
		Discovery: config.ModelDiscovery{Args: []string{"models"}, Format: workerbuiltin.ModelListFormatIDLines, Prefix: "groq/"},
	}
	got := h.d.DiscoverModels(context.Background(), groq)
	want := []config.DiscoveredModel{
		{ID: "groq/openai/gpt-oss-120b", Label: "groq/openai/gpt-oss-120b"},
		{ID: "groq/llama-3.3-70b-versatile", Label: "groq/llama-3.3-70b-versatile"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groq models = %+v, want %+v", got, want)
	}

	// The same binary with a different prefix is a different cache entry.
	cerebras := groq
	cerebras.Provider, cerebras.Discovery.Prefix = "cerebras", "cerebras/"
	if got := h.d.DiscoverModels(context.Background(), cerebras); len(got) != 1 || got[0].ID != "cerebras/gpt-oss-120b" {
		t.Fatalf("cerebras models = %+v", got)
	}
	if len(h.listing.calls) != 2 {
		t.Fatalf("listing ran %d times for two prefixes, want 2", len(h.listing.calls))
	}

	// A prefix that matches nothing is a failure (wrong verb / namespace),
	// not an empty catalog.
	none := groq
	none.Provider, none.Discovery.Prefix = "nothing", "nothing/"
	if got := h.d.DiscoverModels(context.Background(), none); got != nil {
		t.Fatalf("unmatched prefix returned %+v, want nil", got)
	}
}

func TestProviderModelDiscoveryWorksWithoutCacheDir(t *testing.T) {
	// No writable cache dir (unknown home, read-only runtime): discovery
	// still runs and memoizes in-process, silently skipping persistence.
	h := newDiscoveryHarness(t, "")
	if got := h.d.DiscoverModels(context.Background(), cursorRequest); !reflect.DeepEqual(got, wantCursorModels) {
		t.Fatalf("models = %+v", got)
	}
	h.d.DiscoverModels(context.Background(), cursorRequest)
	if len(h.listing.calls) != 1 {
		t.Fatalf("listing ran %d times without a cache dir, want 1 (memoized)", len(h.listing.calls))
	}
	if len(h.logs) != 0 {
		t.Fatalf("missing cache dir logged: %v", h.logs)
	}
}

func TestProviderModelDiscoveryIgnoresCorruptCacheFile(t *testing.T) {
	cacheDir := t.TempDir()
	h := newDiscoveryHarness(t, cacheDir)
	h.d.DiscoverModels(context.Background(), cursorRequest)
	entries, _ := os.ReadDir(cacheDir)
	if err := os.WriteFile(filepath.Join(cacheDir, entries[0].Name()), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := h.newDiscovery(cacheDir)
	if got := second.DiscoverModels(context.Background(), cursorRequest); !reflect.DeepEqual(got, wantCursorModels) {
		t.Fatalf("models after corrupt cache = %+v", got)
	}
	if len(h.listing.calls) != 2 {
		t.Fatalf("corrupt cache file was not treated as a miss: %d runs", len(h.listing.calls))
	}
	// The corrupt file was rewritten with a valid entry.
	data, err := os.ReadFile(filepath.Join(cacheDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var entry modelDiscoveryCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil || !entry.OK || len(entry.Models) != 2 {
		t.Fatalf("rewritten cache entry = %s (err %v)", data, err)
	}
}

func TestModelListingExtraEnvForwardsPathAndCredentials(t *testing.T) {
	env := map[string]string{
		"PATH":           "/opt/tools/bin:/usr/bin",
		"CURSOR_API_KEY": "key-1",
		"XAI_API_KEY":    "",
	}
	got := modelListingExtraEnv([]string{"CURSOR_API_KEY", "XAI_API_KEY", "UNSET_VAR"}, func(k string) string { return env[k] })
	want := []string{"CURSOR_API_KEY=key-1", "PATH=/opt/tools/bin:/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extra env = %v, want %v", got, want)
	}
	if got := modelListingExtraEnv(nil, func(string) string { return "" }); got != nil {
		t.Fatalf("empty PATH and no passthrough produced %v, want nil", got)
	}
}

// pickerDiscoverer is a scripted config.ModelDiscoverer for handler tests.
type pickerDiscoverer struct {
	requests []config.ModelDiscoveryRequest
	models   map[string][]config.DiscoveredModel
}

func (p *pickerDiscoverer) DiscoverModels(_ context.Context, req config.ModelDiscoveryRequest) []config.DiscoveredModel {
	p.requests = append(p.requests, req)
	return p.models[req.Provider]
}

func TestProviderPublicListSurfacesDiscoveredModels(t *testing.T) {
	disc := &pickerDiscoverer{models: map[string][]config.DiscoveredModel{
		"cursor": {{ID: "claude-opus-5-5-high", Label: "Claude Opus 5.5 1M"}},
		"grok":   {{ID: "grok-4.8", Label: "grok-4.8"}},
	}}
	t.Cleanup(config.SetModelDiscoverer(disc))

	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	fetch := func(query string) map[string]ProviderPublicResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/providers/public"+query), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body ProviderPublicListBody
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := make(map[string]ProviderPublicResponse, len(body.Items))
		for _, item := range body.Items {
			out[item.Name] = item
		}
		return out
	}
	choiceValues := func(p ProviderPublicResponse) map[string]string {
		labels := map[string]string{}
		for _, opt := range p.OptionsSchema {
			if opt.Key != "model" {
				continue
			}
			for _, c := range opt.Choices {
				labels[c.Value] = c.Label
			}
		}
		return labels
	}

	providers := fetch("")
	if got := choiceValues(providers["cursor"]); got["claude-opus-5-5-high"] != "Claude Opus 5.5 1M" || got["auto"] == "" {
		t.Fatalf("cursor picker choices = %v, want curated seed plus the discovered id", got)
	}
	if got := choiceValues(providers["grok"]); got["grok-4.8"] == "" || got["grok-4.6"] == "" {
		t.Fatalf("grok picker choices = %v", got)
	}
	// Providers without a listing verb are untouched and never queried.
	for _, req := range disc.requests {
		if req.Provider == "claude" || req.Provider == "codex" {
			t.Fatalf("discoverer queried a curated-only provider: %+v", req)
		}
		if req.Fresh {
			t.Fatalf("default read requested a fresh listing: %+v", req)
		}
	}
	if len(disc.requests) == 0 {
		t.Fatal("discoverer was never queried")
	}

	// ?fresh=true forwards the bypass, like the readiness probes.
	disc.requests = nil
	fetch("?fresh=true")
	if len(disc.requests) == 0 {
		t.Fatal("fresh read did not query the discoverer")
	}
	for _, req := range disc.requests {
		if !req.Fresh {
			t.Fatalf("fresh read produced a non-fresh request: %+v", req)
		}
	}
}

func TestProviderPublicListWithoutDiscovererIsCurated(t *testing.T) {
	t.Cleanup(config.SetModelDiscoverer(nil))
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/providers/public"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body ProviderPublicListBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := config.BuiltinProviders()["cursor"].OptionsSchema
	for _, item := range body.Items {
		if item.Name != "cursor" {
			continue
		}
		for _, opt := range item.OptionsSchema {
			if opt.Key != "model" {
				continue
			}
			var curated []config.OptionChoice
			for _, o := range want {
				if o.Key == "model" {
					curated = o.Choices
				}
			}
			if len(opt.Choices) != len(curated) {
				t.Fatalf("cursor model choices = %d, want the curated %d", len(opt.Choices), len(curated))
			}
			return
		}
	}
	t.Fatal("cursor model option not found in public providers")
}
