#!/usr/bin/env bash
# Install lkmabt into a writable directory on PATH, preferring GOBIN, then
# GOPATH/bin, then conventional user directories. When no PATH directory is
# writable, install into $HOME/.local/bin and print the PATH change instead.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

if ! command -v go >/dev/null 2>&1; then
	printf 'error: go is required to install lkmabt\n' >&2
	exit 1
fi

on_path() {
	case ":$PATH:" in
	*":$1:"*) return 0 ;;
	*) return 1 ;;
	esac
}

# Candidate order: GOBIN, GOPATH/bin, ~/.local/bin, ~/bin, /usr/local/bin.
# The first candidate that is already on PATH and writable wins.
candidates=()
if gobin=$(go env GOBIN) && [ -n "$gobin" ]; then
	candidates+=("$gobin")
fi
if gopath=$(go env GOPATH) && [ -n "$gopath" ]; then
	# GOPATH may be a colon-separated list; only its first entry holds bin/.
	candidates+=("${gopath%%:*}/bin")
fi
if [ -n "${HOME:-}" ]; then
	candidates+=("$HOME/.local/bin" "$HOME/bin")
fi
candidates+=("/usr/local/bin")

target=
for directory in "${candidates[@]}"; do
	[ -n "$directory" ] || continue
	on_path "$directory" || continue
	if [ ! -d "$directory" ]; then
		mkdir -p "$directory" 2>/dev/null || continue
	fi
	[ -w "$directory" ] || continue
	target=$directory
	break
done

# Second pass: any existing, writable, absolute directory already on PATH,
# regardless of naming convention. Installing there still beats requiring a
# PATH change.
if [ -z "$target" ]; then
	path_entries=()
	IFS=: read -r -a path_entries <<< "$PATH"
	for directory in "${path_entries[@]}"; do
		[ -n "$directory" ] || continue
		case "$directory" in /*) ;; *) continue ;; esac
		[ -d "$directory" ] || continue
		[ -w "$directory" ] || continue
		target=$directory
		break
	done
fi

# No PATH directory is writable: install into $HOME/.local/bin and report the
# one-line PATH change instead of silently leaving lkmabt unusable.
if [ -z "$target" ]; then
	if [ -z "${HOME:-}" ]; then
		printf 'error: no writable directory on PATH and HOME is unset\n' >&2
		exit 1
	fi
	target="$HOME/.local/bin"
	mkdir -p "$target"
fi

printf 'installing lkmabt in %s\n' "$target"
# GOWORK=off ignores any enclosing go.work so the install builds this module.
(
	cd "$repo_root"
	GOWORK=off GOBIN="$target" go install ./cmd/lkmabt
)

resolved=$(command -v lkmabt 2>/dev/null || true)
if [ "$resolved" = "$target/lkmabt" ]; then
	printf 'installed lkmabt in %s\n' "$target"
	exit 0
fi

cat >&2 <<EOF
error: lkmabt was installed in $target, but that is not the lkmabt command on PATH.
Add the directory to your shell profile and start a new shell:
  export PATH="$target:\$PATH"
EOF
exit 1
