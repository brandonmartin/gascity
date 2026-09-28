package builtin

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Model listing formats a provider CLI can print. A BuiltinModelDiscovery
// names the one its Args produce; ParseModelListing dispatches on it.
//
// Per-provider findings (ga-k67, verified 2026-09-25 unless noted):
//
//   - cursor-agent 2026.09.15: `--list-models` prints "<id> - <label>" body
//     lines under an "Available models" header. Verified.
//   - grok 1.0.41: `grok models` prints "  * <id> (default)" / "  - <id>"
//     bullets under "Available models:". Verified.
//   - opencode: `opencode models` prints one "<provider>/<model>" id per line
//     (documented CLI verb; not run locally). mimocode is an opencode fork
//     and inherits the verb.
//   - codex-cli 0.157.0: no listing verb (`codex --help` has neither a models
//     subcommand nor --list-models; a bare `codex models` starts the TUI).
//   - claude 2.1.282: no listing verb (`claude --help` publishes none).
//   - gemini, kimi, kiro, copilot, amp, auggie, pi, omp, antigravity: no
//     known listing verb; not installed locally to verify. Curated only.
const (
	// ModelListFormatCursor parses "<id> - <label>" body lines.
	ModelListFormatCursor = "cursor-list-models"
	// ModelListFormatGrok parses "* <id> (default)" / "- <id>" bullet lines.
	ModelListFormatGrok = "grok-models"
	// ModelListFormatIDLines parses one bare model id per line.
	ModelListFormatIDLines = "id-lines"
)

// Sentinel errors from ParseModelListing. Both mean "keep the curated seed":
// discovery is advisory and never removes or replaces curated entries.
var (
	// ErrUnknownModelListFormat reports a format id with no parser.
	ErrUnknownModelListFormat = errors.New("unknown model listing format")
	// ErrNoModelsParsed reports output that yielded no model id at all — a
	// wrong verb, a logged-out CLI, or a changed output format, never an
	// empty catalog.
	ErrNoModelsParsed = errors.New("model listing produced no model ids")
)

// BuiltinModelDiscovery declares how a builtin provider's CLI lists the model
// ids it accepts, so the curated model choices in OptionsSchema become a
// fallback seed rather than the whole catalog. Nil on a spec means the CLI
// publishes no listing verb and the curated list is all there is.
//
//nolint:revive // Mirrors the config boundary naming intentionally.
type BuiltinModelDiscovery struct {
	// Args are appended to the provider Command to print the listing
	// (e.g. {"--list-models"} or {"models"}).
	Args []string
	// Format names the ModelListFormat* parser for the output.
	Format string
	// Prefix, when set, keeps only ids that start with it. opencode-family
	// providers share one binary whose listing spans every configured
	// upstream; the prefix scopes it to this provider's namespace.
	Prefix string
}

// DiscoveredModel is one model id a provider CLI reported, with the display
// label it printed for it (or the id itself when the listing has no labels).
type DiscoveredModel struct {
	ID    string
	Label string
}

// ModelListFormats returns every listing format ParseModelListing accepts.
func ModelListFormats() []string {
	return []string{ModelListFormatCursor, ModelListFormatGrok, ModelListFormatIDLines}
}

// ParseModelListing parses a provider CLI's model listing into discovered
// models, dropping header, banner, tip, and malformed lines, and collapsing
// repeated ids to their first occurrence. It returns ErrNoModelsParsed when
// the output yields no id at all.
func ParseModelListing(format, out string) ([]DiscoveredModel, error) {
	var models []DiscoveredModel
	switch format {
	case ModelListFormatCursor:
		for _, model := range parseCursorModelList(out) {
			models = append(models, DiscoveredModel(model))
		}
	case ModelListFormatGrok:
		models = parseGrokModelList(out)
	case ModelListFormatIDLines:
		models = parseModelIDLines(out)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownModelListFormat, format)
	}
	models = dedupeDiscoveredModels(models)
	if len(models) == 0 {
		return nil, ErrNoModelsParsed
	}
	return models, nil
}

// parseGrokModelList parses `grok models` output: bullet lines of the form
// "  * <id> (default)" for the default model and "  - <id>" for the rest.
// Everything without a bullet (login banner, "Default model:", the header)
// is skipped.
func parseGrokModelList(out string) []DiscoveredModel {
	var models []DiscoveredModel
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		var rest string
		switch {
		case strings.HasPrefix(line, "* "):
			rest = line[2:]
		case strings.HasPrefix(line, "- "):
			rest = line[2:]
		default:
			continue
		}
		id, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
		if !isModelID(id) {
			continue
		}
		models = append(models, DiscoveredModel{ID: id, Label: id})
	}
	return models
}

// parseModelIDLines parses a listing that prints one bare model id per line.
// Any line that is not a single id token is skipped.
func parseModelIDLines(out string) []DiscoveredModel {
	var models []DiscoveredModel
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		id := strings.TrimSpace(scanner.Text())
		if !isModelID(id) {
			continue
		}
		models = append(models, DiscoveredModel{ID: id, Label: id})
	}
	return models
}

// isModelID reports whether token is shaped like a model id: a single
// non-empty token with no whitespace or control characters that does not
// end in ':' (a header) or start with a punctuation the CLIs use for prose.
func isModelID(token string) bool {
	if token == "" || len(token) > 256 {
		return false
	}
	if strings.HasSuffix(token, ":") {
		return false
	}
	first := rune(token[0])
	if !unicode.IsLetter(first) && !unicode.IsDigit(first) {
		return false
	}
	for _, r := range token {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func dedupeDiscoveredModels(models []DiscoveredModel) []DiscoveredModel {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]DiscoveredModel, 0, len(models))
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		if _, ok := seen[model.ID]; ok {
			continue
		}
		seen[model.ID] = struct{}{}
		if model.Label == "" {
			model.Label = model.ID
		}
		out = append(out, model)
	}
	return out
}

func cloneBuiltinModelDiscovery(discovery *BuiltinModelDiscovery) *BuiltinModelDiscovery {
	if discovery == nil {
		return nil
	}
	return &BuiltinModelDiscovery{
		Args:   cloneStrings(discovery.Args),
		Format: discovery.Format,
		Prefix: discovery.Prefix,
	}
}
