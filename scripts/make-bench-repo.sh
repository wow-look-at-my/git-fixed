#!/bin/bash
# Builds a synthetic repository large enough for a meaningful fsck timing.
#
# Usage: scripts/make-bench-repo.sh <dir> [commits] [files] [churn]
#
# The history goes in through git fast-import, because building it one commit
# at a time spends nearly all its time starting processes.
set -euo pipefail

dir=${1:?usage: make-bench-repo.sh <dir> [commits] [files] [churn]}
commits=${2:-2000}
files=${3:-500}
churn=${4:-40}
here=$(cd "$(dirname "$0")" && pwd)

rm -rf "$dir"
mkdir -p "$dir"
git init -q --bare "$dir"
node "$here/gen-import.js" "$commits" "$files" "$churn" |
	git -C "$dir" fast-import --quiet --done
git -C "$dir" repack -adq
git -C "$dir" count-objects -v
