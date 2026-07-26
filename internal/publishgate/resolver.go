package publishgate

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/git"
)

// DefaultResolveTimeout bounds each git invocation. Every call is a local
// object-store read, so anything slower than this means the repository is
// wedged and the caller is better off reporting "unknown" than blocking.
const DefaultResolveTimeout = 5 * time.Second

// ErrUnresolvedRef is returned when a ref does not resolve in the repository.
var ErrUnresolvedRef = errors.New("ref does not resolve")

// GitResolver resolves refs against a real repository using local refs only.
type GitResolver struct {
	dir string
}

// NewGitResolver returns a Resolver rooted at dir. dir may be any path
// inside the repository, including a worktree.
func NewGitResolver(dir string) *GitResolver {
	return &GitResolver{dir: dir}
}

// ResolveRef returns the commit SHA the ref names. Refs that do not resolve
// locally return ErrUnresolvedRef; the caller decides whether that is a
// missing artifact or merely an unfetched repository.
func (g *GitResolver) ResolveRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if err := validateRevArg(ref); err != nil {
		return "", err
	}
	out, err := g.run("rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || out == "" {
		return "", fmt.Errorf("%w: %s", ErrUnresolvedRef, ref)
	}
	return out, nil
}

// CountBehind returns how many commits `to` has that `from` does not.
func (g *GitResolver) CountBehind(from, to string) (int, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if err := validateRevArg(from); err != nil {
		return 0, err
	}
	if err := validateRevArg(to); err != nil {
		return 0, err
	}
	out, err := g.run("rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	n, convErr := strconv.Atoi(out)
	if convErr != nil {
		return 0, fmt.Errorf("unexpected rev-list output %q: %w", out, convErr)
	}
	return n, nil
}

// CurrentBranch returns the checked-out branch name. A detached HEAD or a
// non-repository directory is an error, not an empty branch: callers use
// this to prove they are standing in a specific worktree.
func (g *GitResolver) CurrentBranch() (string, error) {
	out, err := g.run("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolving current branch in %s: %w", g.dir, err)
	}
	if out == "" {
		return "", fmt.Errorf("no branch checked out in %s", g.dir)
	}
	return out, nil
}

func (g *GitResolver) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultResolveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	// Strip inherited GIT_DIR/GIT_WORK_TREE so refs resolve against dir and
	// not against whatever repository the calling process was launched from.
	cmd.Env = git.SanitizedEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// validateRevArg rejects revision arguments that git would read as options.
// Bead metadata is written by agents and is not trusted input to a
// subprocess argv.
func validateRevArg(rev string) error {
	rev = strings.TrimSpace(rev)
	switch {
	case rev == "":
		return fmt.Errorf("empty revision")
	case strings.HasPrefix(rev, "-"):
		return fmt.Errorf("refusing option-shaped revision %q", rev)
	case strings.ContainsAny(rev, " \t\n"):
		return fmt.Errorf("refusing revision with whitespace %q", rev)
	}
	return nil
}
