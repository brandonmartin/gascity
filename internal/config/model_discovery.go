package config

import (
	"context"
	"sync"

	workerbuiltin "github.com/gastownhall/gascity/internal/worker/builtin"
)

// ModelOptionKey is the options-schema key model discovery merges into.
const ModelOptionKey = "model"

// ModelDiscovery declares how a provider CLI lists the model ids it accepts.
// When set, the curated choices of the provider's open "model" option are a
// fallback seed: at resolve time and on every picker read the binary's own
// listing is merged in, so a model the CLI gained after this release is
// selectable without a code change (ga-k67). Nil means the CLI publishes no
// listing verb and the curated list is the whole catalog.
//
//	[providers.mycli.model_discovery]
//	args   = ["--list-models"]   # appended to the provider command
//	format = "id-lines"          # one of the ModelListFormat* parsers
//	prefix = "my/"               # optional: keep only ids under this namespace
type ModelDiscovery struct {
	// Args are appended to the provider command to print the listing.
	Args []string `toml:"args" json:"args"`
	// Format names the listing parser: "cursor-list-models" ("<id> - <label>"
	// lines), "grok-models" ("* <id>" / "- <id>" bullets) or "id-lines" (one
	// bare id per line).
	Format string `toml:"format" json:"format" jsonschema:"enum=cursor-list-models,enum=grok-models,enum=id-lines"`
	// Prefix, when set, keeps only ids that start with it. Providers that
	// share one binary across upstream namespaces (the opencode family) use it
	// to scope a listing that spans every configured upstream.
	Prefix string `toml:"prefix,omitempty" json:"prefix,omitempty"`
}

func (d *ModelDiscovery) clone() *ModelDiscovery {
	if d == nil {
		return nil
	}
	return &ModelDiscovery{
		Args:   cloneStrings(d.Args),
		Format: d.Format,
		Prefix: d.Prefix,
	}
}

// DiscoveredModel is one model id a provider CLI reported, with the display
// label it printed (or the id itself when the listing carries no labels).
type DiscoveredModel struct {
	ID    string
	Label string
}

// ModelDiscoveryRequest is what a ModelDiscoverer needs to run one listing.
type ModelDiscoveryRequest struct {
	// Provider is the resolved provider name, for diagnostics and cache keys.
	Provider string
	// Binary is the executable name to resolve on PATH — the same token
	// provider resolution checks with LookPathFunc, so discovery queries the
	// binary a session would actually launch.
	Binary string
	// Discovery is the listing declaration to run.
	Discovery ModelDiscovery
	// PassthroughEnv names environment variables to forward from the current
	// process (the harness's credential/base-URL bindings), because a listing
	// is account-scoped and headless installs log in through the environment.
	PassthroughEnv []string
	// Fresh bypasses every cache layer and re-runs the binary.
	Fresh bool
}

// ModelDiscoverer runs a provider's listing verb and returns the ids it
// printed. Discovery is advisory: an implementation returns nil for any
// failure (binary absent, exec error, timeout, unparseable output) and never
// returns an error, so resolution and the picker fall back to the curated
// seed. Process spawning is a Layer-0 side effect, so this package holds only
// the seam; the implementation is injected by the process entry point (see
// SetModelDiscoverer) exactly like LookPathFunc is injected into
// ResolveProvider.
type ModelDiscoverer interface {
	DiscoverModels(ctx context.Context, req ModelDiscoveryRequest) []DiscoveredModel
}

var (
	modelDiscovererMu sync.RWMutex
	modelDiscoverer   ModelDiscoverer
)

// SetModelDiscoverer installs the process-wide ModelDiscoverer and returns a
// func that restores the previous one. Nil disables discovery (the default),
// which keeps every resolved schema byte-identical to the curated catalog —
// the state every test process runs in unless it installs a fake.
func SetModelDiscoverer(d ModelDiscoverer) (restore func()) {
	modelDiscovererMu.Lock()
	previous := modelDiscoverer
	modelDiscoverer = d
	modelDiscovererMu.Unlock()
	return func() {
		modelDiscovererMu.Lock()
		modelDiscoverer = previous
		modelDiscovererMu.Unlock()
	}
}

func currentModelDiscoverer() ModelDiscoverer {
	modelDiscovererMu.RLock()
	defer modelDiscovererMu.RUnlock()
	return modelDiscoverer
}

// MergeDiscoveredModelChoices returns schema with discovered ids appended to
// its open "model" option. It is pure: the input schema is never mutated, and
// it is returned as-is when there is nothing to merge.
//
// Precedence: a curated choice always wins on collision, so aliases keep
// their expansion ("opus" -> --model claude-opus-4-8), labels stay curated,
// and FlagAliases used for arg stripping survive. Discovered-only ids are
// appended in listing order as plain entries whose FlagArgs render the
// option's FlagTemplate — the same flags the open-enum passthrough would emit
// for them, so pinning behaves identically whether or not discovery ran.
func MergeDiscoveredModelChoices(schema []ProviderOption, discovered []DiscoveredModel) []ProviderOption {
	if len(discovered) == 0 {
		return schema
	}
	index := -1
	for i := range schema {
		if schema[i].Key == ModelOptionKey {
			index = i
			break
		}
	}
	if index < 0 || !IsOpenOption(schema[index]) {
		return schema
	}
	opt := schema[index]
	known := make(map[string]struct{}, len(opt.Choices)+len(discovered))
	for _, choice := range opt.Choices {
		known[choice.Value] = struct{}{}
	}
	var added []OptionChoice
	for _, model := range discovered {
		if model.ID == "" {
			continue
		}
		if _, ok := known[model.ID]; ok {
			continue
		}
		known[model.ID] = struct{}{}
		label := model.Label
		if label == "" {
			label = model.ID
		}
		added = append(added, OptionChoice{
			Value:    model.ID,
			Label:    label,
			FlagArgs: renderFlagTemplate(opt.FlagTemplate, model.ID),
		})
	}
	if len(added) == 0 {
		return schema
	}
	out := deepCopyProviderOptions(schema)
	out[index].Choices = append(out[index].Choices, added...)
	return out
}

// DiscoverProviderOptions returns spec's options schema with the ids the
// provider binary reports merged into its model option. It is the picker's
// read path: nothing is cached on spec, so each read reflects the current
// listing, and fresh re-runs the binary past every cache layer. Without a
// discoverer installed, or for a provider that declares no listing verb, the
// curated schema is returned unchanged.
func DiscoverProviderOptions(ctx context.Context, name string, spec ProviderSpec, fresh bool) []ProviderOption {
	return discoverModelChoices(ctx, name, spec.pathCheckBinary(), spec.ModelDiscovery, spec.UpstreamEnv, spec.OptionsSchema, fresh)
}

// applyDiscoveredModels is the resolve-time counterpart of
// DiscoverProviderOptions: it widens rp's model option in place.
func applyDiscoveredModels(rp *ResolvedProvider, binary string) {
	rp.OptionsSchema = discoverModelChoices(context.Background(), rp.Name, binary, rp.ModelDiscovery, rp.UpstreamEnv, rp.OptionsSchema, false)
}

func discoverModelChoices(ctx context.Context, name, binary string, discovery *ModelDiscovery, upstream UpstreamEnvBinding, schema []ProviderOption, fresh bool) []ProviderOption {
	if discovery == nil || binary == "" {
		return schema
	}
	discoverer := currentModelDiscoverer()
	if discoverer == nil {
		return schema
	}
	opt := findOption(schema, ModelOptionKey)
	if opt == nil || !IsOpenOption(*opt) {
		return schema
	}
	models := discoverer.DiscoverModels(ctx, ModelDiscoveryRequest{
		Provider:       name,
		Binary:         binary,
		Discovery:      *discovery.clone(),
		PassthroughEnv: upstreamEnvNames(upstream),
		Fresh:          fresh,
	})
	return MergeDiscoveredModelChoices(schema, models)
}

// upstreamEnvNames lists the non-empty env-var names of a harness binding in
// a stable order (API key, auth token, base URL).
func upstreamEnvNames(upstream UpstreamEnvBinding) []string {
	var names []string
	for _, name := range []string{upstream.APIKey, upstream.AuthToken, upstream.BaseURL} {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func modelDiscoveryFromWorker(discovery *workerbuiltin.BuiltinModelDiscovery) *ModelDiscovery {
	if discovery == nil {
		return nil
	}
	return &ModelDiscovery{
		Args:   cloneStrings(discovery.Args),
		Format: discovery.Format,
		Prefix: discovery.Prefix,
	}
}
