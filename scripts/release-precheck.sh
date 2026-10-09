#!/usr/bin/env bash
# scripts/release-precheck.sh — refuse every wrong release tag before the owner approval gate.
#
# Usage (TAG comes from the environment; GITHUB_OUTPUT is optional, Actions sets it):
#   TAG=v0.1.0 scripts/release-precheck.sh
#
# Prints (and, when GITHUB_OUTPUT is set, writes) "version=<X.Y.Z>" and
# "sha=<tagged commit>". Exit codes: 2 = the tag is not a plain vX.Y.Z tag;
# 1 = a precondition failed (version.go does not say "v" + the tag, no dated
# CHANGELOG entry, the tag does not resolve to a commit, or that commit is not
# on origin/main — D-14, D-18).
#
# Whether the tag is annotated and whether the tagged commit has green CI on
# main are checked by release.yml through the GitHub API, because a checkout of
# a tag ref may not keep the tag object. Nothing here contacts the module proxy
# or pkg.go.dev: the first fetch of a version is permanent (D-18), so it must
# only ever follow the owner's approval.
#
# The tag is attacker-influenced text that reaches this script, so it is only
# ever matched against an anchored regex and never echoed back (T-12-24).
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

TAG_VALUE=${TAG:-}
TAG_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
if ! [[ "$TAG_VALUE" =~ $TAG_RE ]]; then
  echo "release-precheck: TAG is not a vX.Y.Z tag" >&2
  exit 2
fi

VERSION=${TAG_VALUE#v}

# The committed Version constant must already be the tag's version.
FILE_VERSION=$(sed -nE 's/^const Version = "([^"]+)"$/\1/p' "$ROOT/version.go")
if [ "$FILE_VERSION" != "$VERSION" ]; then
  echo "release-precheck: version.go does not say \"v\" + the tag's version" >&2
  exit 1
fi

# The CHANGELOG entry is reviewed on main before any tag is pushed.
VERSION_ESCAPED=${VERSION//./\\.}
if ! grep -Eq "^## \\[$VERSION_ESCAPED\\] - [0-9]{4}-[0-9]{2}-[0-9]{2}\$" "$ROOT/CHANGELOG.md"; then
  echo "release-precheck: CHANGELOG.md has no dated entry for the tag's version" >&2
  exit 1
fi

if ! SHA=$(git -C "$ROOT" rev-parse --verify "refs/tags/$TAG_VALUE^{commit}" 2>/dev/null); then
  echo "release-precheck: the tag does not resolve to a commit" >&2
  exit 1
fi

if ! git -C "$ROOT" rev-parse --verify --quiet "refs/remotes/origin/main^{commit}" >/dev/null; then
  echo "release-precheck: origin/main is not available in this checkout" >&2
  exit 1
fi
if ! git -C "$ROOT" merge-base --is-ancestor "$SHA" "refs/remotes/origin/main"; then
  echo "release-precheck: the tagged commit is not on origin/main" >&2
  exit 1
fi

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    printf 'version=%s\n' "$VERSION"
    printf 'sha=%s\n' "$SHA"
  } >> "$GITHUB_OUTPUT"
fi
printf 'version=%s\n' "$VERSION"
printf 'sha=%s\n' "$SHA"
