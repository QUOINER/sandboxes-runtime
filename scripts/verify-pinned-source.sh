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

if ! source_root=$(CDPATH= cd -- "$source_dir" && pwd -P); then
  echo "source directory does not exist" >&2
  exit 1
fi

# Ignore inherited Git variables and user/system configuration. The Git
# worktree used for validation must be the exact directory the build copies.
git_base() {
  env -i PATH="${PATH:-/usr/bin:/bin}" HOME=/ \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
    git "$@"
}

if ! source_git_dir=$(git_base -C "$source_root" rev-parse --absolute-git-dir 2>/dev/null); then
  echo "source directory is not a git checkout" >&2
  exit 1
fi

git_source() {
  env -i PATH="${PATH:-/usr/bin:/bin}" HOME=/ \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
    GIT_DIR="$source_git_dir" GIT_WORK_TREE="$source_root" \
    git -c core.fsmonitor=false -c core.untrackedCache=false -c core.excludesFile=/dev/null "$@"
}

if ! source_commit=$(git_source rev-parse --verify 'HEAD^{commit}' 2>/dev/null); then
  echo "source directory is not a git checkout" >&2
  exit 1
fi
if [ "$source_commit" != "$pinned_commit" ]; then
  echo "source commit mismatch: got $source_commit, want $pinned_commit" >&2
  exit 1
fi

if git_source config --local --get-regexp '^filter\.' >/dev/null 2>&1; then
  echo "source checkout must not configure Git filters" >&2
  exit 1
else
  config_status=$?
  if [ "$config_status" -ne 1 ]; then
    echo "cannot inspect source Git configuration" >&2
    exit 1
  fi
fi

if ! index_entries=$(git_source ls-files -v); then
  echo "cannot inspect source index flags" >&2
  exit 1
fi
if printf '%s\n' "$index_entries" | grep -Eq '^[a-zS] '; then
  echo "source index has assume-unchanged or skip-worktree entries" >&2
  exit 1
fi

if ! source_changes=$(git_source status --porcelain --untracked-files=all --ignored=matching); then
  echo "cannot inspect source checkout status" >&2
  exit 1
fi
if [ -n "$source_changes" ]; then
  echo "source checkout must be clean" >&2
  exit 1
fi
