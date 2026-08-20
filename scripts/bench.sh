#!/bin/bash
# Compares git-fixed's fsck against the system git on one repository.
#
# Usage: scripts/bench.sh <repo> [runs]
#
# It runs both tools the same number of times, drops the first run of each so
# the page cache is warm for both, and prints the best wall-clock time. It also
# fails when the two disagree, so a fast wrong answer cannot look like a win.
set -euo pipefail

repo=${1:?usage: bench.sh <repo> [runs]}
runs=${2:-3}
ours=$(cd "$(dirname "$0")/.." && pwd)/build/git-fsck

if [ ! -x "$ours" ]; then
	echo "build/git-fsck is missing; run go-toolchain first" >&2
	exit 1
fi

# best runs a command `runs` times and prints the shortest elapsed time in
# seconds. The first run is a warm-up and does not count.
best() {
	local lowest=""
	local i start end elapsed
	for ((i = 0; i <= runs; i++)); do
		start=$(date +%s.%N)
		"$@" >/dev/null 2>&1 || true
		end=$(date +%s.%N)
		[ "$i" -eq 0 ] && continue
		elapsed=$(echo "$end - $start" | bc)
		if [ -z "$lowest" ] || [ "$(echo "$elapsed < $lowest" | bc)" -eq 1 ]; then
			lowest=$elapsed
		fi
	done
	printf '%s' "$lowest"
}

cd "$repo"

# Both tools must agree before any timing means anything.
git fsck >/tmp/bench-git.out 2>/tmp/bench-git.err || true
"$ours" >/tmp/bench-ours.out 2>/tmp/bench-ours.err || true
if ! diff <(sort /tmp/bench-git.out /tmp/bench-git.err) \
	<(sort /tmp/bench-ours.out /tmp/bench-ours.err) >/dev/null; then
	echo "output differs from git fsck; refusing to report a time" >&2
	diff <(sort /tmp/bench-git.out /tmp/bench-git.err) \
		<(sort /tmp/bench-ours.out /tmp/bench-ours.err) >&2 || true
	exit 1
fi

git_time=$(best git fsck)
our_time=$(best "$ours")
one_time=$(best env GIT_FIXED_THREADS=1 "$ours")

printf 'repository: %s\n' "$repo"
printf 'objects:    %s\n' "$(git count-objects -v | awk '/^count:|^in-pack:/ {t += $2} END {print t}')"
printf 'git fsck:            %ss\n' "$git_time"
printf 'git-fixed (1 worker): %ss\n' "$one_time"
printf 'git-fixed:            %ss  (%sx)\n' "$our_time" \
	"$(echo "scale=2; $git_time / $our_time" | bc)"
