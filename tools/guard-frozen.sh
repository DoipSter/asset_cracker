#!/bin/sh
# Fail if frozen archives changed versus origin/main, unless AC_ALLOW_LEGACY=1.
# Frozen: service/internal/legacy, cmd/replay, cmd/replay2, tools/parity.
# If those paths are not on the base (first landing), this is a no-op.

set -eu

if [ "${AC_ALLOW_LEGACY:-}" = "1" ]; then
	exit 0
fi

root=$(git rev-parse --show-toplevel)
cd "$root"

base="${AC_FROZEN_BASE:-origin/main}"
if ! git rev-parse --verify "$base" >/dev/null 2>&1; then
	echo "guard-frozen: no $base in this clone; skipped"
	exit 0
fi

paths="service/internal/legacy service/cmd/replay service/cmd/replay2 tools/parity"
present=
for p in $paths; do
	if git cat-file -e "$base:$p" 2>/dev/null || git ls-tree -d --name-only "$base" "$p" | grep -q .; then
		present="$present $p"
	fi
done
if [ -z "${present# }" ]; then
	echo "guard-frozen: frozen paths are not on $base yet; skipped"
	exit 0
fi

changed=$(git diff --name-only "$base" -- $paths)
if [ -n "$changed" ]; then
	echo "guard-frozen: frozen archives differ from $base:"
	echo "$changed"
	echo "set AC_ALLOW_LEGACY=1 if this change to the replay archives is deliberate"
	exit 1
fi
