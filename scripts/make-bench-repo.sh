#!/bin/bash
# Builds a synthetic repository large enough for a meaningful fsck timing.
#
# Usage: scripts/make-bench-repo.sh <dir> [commits] [files] [churn] [blobsize]
#
# The history goes in through git fast-import, because building it one commit
# at a time spends nearly all its time starting processes.
#
# blobsize pads each blob with data that does not compress, for a repository
# whose packfile is large next to its object count. That is the shape that says
# what a run costs the machine rather than the collector. see docs/memory.md
set -euo pipefail

dir=${1:?usage: make-bench-repo.sh <dir> [commits] [files] [churn] [blobsize]}
commits=${2:-2000}
files=${3:-500}
churn=${4:-40}
blobsize=${5:-0}
here=$(cd "$(dirname "$0")" && pwd)

rm -rf "$dir"
mkdir -p "$dir"
git init -q --bare "$dir"
node "$here/gen-import.js" "$commits" "$files" "$churn" "$blobsize" |
	git -C "$dir" fast-import --quiet --done
git -C "$dir" repack -adq
git -C "$dir" count-objects -v
