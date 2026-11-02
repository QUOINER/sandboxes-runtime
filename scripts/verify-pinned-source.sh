#!/bin/sh
# Reject a source tree that is not the exact clean upstream commit in manifest.yaml.
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: verify-pinned-source.sh SOURCE_DIR COMMIT" >&2
  exit 2
fi

source_dir=$1
pinned_commit=$2
case "$pinned_commit" in
  ''|*[!0-9a-f]*)
    echo "expected commit must be a lowercase 40-character git hash" >&2
    exit 2
    ;;
esac
if [ "${#pinned_commit}" -ne 40 ]; then
  echo "expected commit must be a lowercase 40-character git hash" >&2
  exit 2
fi

if ! source_commit=$(git -C "$source_dir" rev-parse --verify 'HEAD^{commit}' 2>/dev/null); then
  echo "source directory is not a git checkout" >&2
  exit 1
fi
if [ "$source_commit" != "$pinned_commit" ]; then
  echo "source commit mismatch: got $source_commit, want $pinned_commit" >&2
  exit 1
fi
if ! source_changes=$(git -C "$source_dir" status --porcelain --untracked-files=all --ignored=matching); then
  echo "cannot inspect source checkout status" >&2
  exit 1
fi
if [ -n "$source_changes" ]; then
  echo "source checkout must be clean" >&2
  exit 1
fi
