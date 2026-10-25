#!/usr/bin/env bash
# Verify .githooks is this clone's active core.hooksPath.
#
# The gates in .githooks cannot report their own absence: when another installer
# claims core.hooksPath (beads points it at .beads/hooks), git stops invoking
# them entirely and every commit looks clean while formatting, lint-changed (nogo
# lint and vet) and the codegen+stage steps are all skipped. That is how spec-derived
# drift reached the mainline. This check is the external detector; run it
# with `make check-hooks`.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
configured="$(git config --get core.hooksPath || true)"

expected="$repo_root/.githooks"

if [ ! -d "$expected" ]; then
	echo "ERROR: $expected does not exist — this does not look like a gascity clone." >&2
	exit 1
fi

if [ -z "$configured" ]; then
	actual=""
else
	# core.hooksPath may be relative to the repo root or absolute. Resolve both
	# spellings to a canonical path before comparing.
	case "$configured" in
	/*) candidate="$configured" ;;
	*) candidate="$repo_root/$configured" ;;
	esac
	actual="$(cd "$candidate" 2>/dev/null && pwd -P || true)"
fi

expected_actual="$(cd "$expected" && pwd -P)"
if [ "$actual" = "$expected_actual" ]; then
	exit 0
fi

# The rig identity override installs a private dispatcher so its post-commit,
# post-merge, and pre-push hooks can record seat notes while still chaining the
# repository hooks. Accept that owner only when it delegates to this clone's
# .githooks and has an executable shim for every repository hook.
identity_problem=""
if [ -n "$actual" ] && [ -x "$actual/gc-identity-dispatch" ]; then
	identity_chain="$("$actual/gc-identity-dispatch" --chain-dir 2>/dev/null || true)"
	if [ -z "$identity_chain" ]; then
		identity_problem="gc-identity-dispatch did not report a chain directory"
	else
		case "$identity_chain" in
		/*) identity_candidate="$identity_chain" ;;
		*) identity_candidate="$repo_root/$identity_chain" ;;
		esac
		identity_actual="$(cd "$identity_candidate" 2>/dev/null && pwd -P || true)"
		if [ "$identity_actual" != "$expected_actual" ]; then
			identity_problem="gc-identity-dispatch chains to ${identity_actual:-$identity_chain}, expected $expected_actual"
		else
			for hook_path in "$expected"/*; do
				[ -f "$hook_path" ] || continue
				hook_name="${hook_path##*/}"
				if [ ! -x "$actual/$hook_name" ]; then
					identity_problem="gc-identity-dispatch is missing an executable shim for .githooks/$hook_name"
					break
				fi
			done
			if [ -z "$identity_problem" ]; then
				exit 0
			fi
		fi
	fi
fi

{
	echo "ERROR: .githooks is not this clone's git hooks directory."
	echo "  core.hooksPath: ${configured:-<unset — git defaults to .git/hooks>}"
	echo "  expected:       $expected"
	if [ -n "$identity_problem" ]; then
		echo "  identity chain: $identity_problem"
	fi
	echo
	echo "Every gate in .githooks (staged-Go formatting, nogo lint-changed, the OpenAPI"
	echo "spec/client/schema codegen+stage steps, the push-time suite) is"
	echo "silently skipped while another directory owns core.hooksPath."
	echo
	if [ -n "$identity_problem" ]; then
		echo "Fix it by re-running the rig's git-identity.sh --apply to restore its private hooks."
	else
		echo "Fix it with:  make setup"
		echo
		echo "Beads is not lost by doing this: .githooks chains every beads-managed hook"
		echo "through .githooks/lib/beads-chain.sh. If beads reclaims core.hooksPath"
		echo "later (its installer does), re-run 'make setup'."
	fi
} >&2
exit 1
