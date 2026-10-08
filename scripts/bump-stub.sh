#!/usr/bin/env bash
# scripts/bump-stub.sh — vendor and verify the docql-stub SDK contract kit (CTR-02).
#
# Usage:
#   scripts/bump-stub.sh <pin>     # pull <pin>, boot it, vendor the served kit into contract/
#   scripts/bump-stub.sh --verify  # prove the kit the pinned image serves equals contract/
#
# STUB_PORT (default 18080) is the loopback port the stub container is published on.
#
# D-07: both modes use the same HTTP channel — the pinned image is pulled, booted and
# read through GET /__stub/sdk-cases and GET /__stub/sdk-contract/<name>. There is no
# docker cp: what CI verifies is what a local run verifies.
#
# Bump only when the sdk-cases version changes (D-06): the vendored kit and the pin
# move together in one commit, so the manifest sha256 stays tied to the image digest.
#
# Output: CONTRACT_KIT_OK <n> files on success; CONTRACT_KIT_MISMATCH <name> and
# exit 1 when a served file differs from (or is missing from) the vendored copy.
# The DOCQL_INTERNAL_KEY is generated at runtime and never printed (T-10-02).
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
CONTRACT_DIR="$ROOT/contract"
PORT=${STUB_PORT:-18080}
CONTAINER="docql-sdk-kit-$$"
TMP=$(mktemp -d)
trap 'docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; rm -rf "$TMP"' EXIT

PIN_RE='^ghcr\.io/finoniq/docql-stub:sha-[0-9a-f]{7}@sha256:[0-9a-f]{64}$'
usage() {
  echo "usage: scripts/bump-stub.sh <pin> | scripts/bump-stub.sh --verify" >&2
  echo "  <pin> must match ghcr.io/finoniq/docql-stub:sha-<7 hex>@sha256:<64 hex>" >&2
  exit 2
}

[ "$#" -eq 1 ] || usage
if [ "$1" = "--verify" ]; then
  MODE=verify
  if [ ! -f "$CONTRACT_DIR/stub-image" ]; then
    echo "contract/stub-image is missing — run scripts/bump-stub.sh <pin> first" >&2
    exit 2
  fi
  PIN=$(cat "$CONTRACT_DIR/stub-image")
else
  MODE=bump
  PIN=$1
fi

if ! [[ "$PIN" =~ $PIN_RE ]]; then
  echo "invalid pin: $PIN" >&2
  echo "expected: ghcr.io/finoniq/docql-stub:sha-<7 hex>@sha256:<64 hex>" >&2
  exit 2
fi

# Fresh random key for this run only; never echoed (T-10-02).
INTERNAL_KEY=$(openssl rand -hex 24)

ENV_FLAGS=()
if [ "$MODE" = verify ]; then
  # The vendored stubEnv recipe, one KEY=VALUE line per entry: --verify proves the
  # kit's own recipe boots the image (D-07).
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    ENV_FLAGS+=(-e "$line")
  done < <(jq -r '.stubEnv | to_entries[] | "\(.key)=\(.value)"' "$CONTRACT_DIR/sdk-cases.json")
fi

docker pull "$PIN" >/dev/null

docker run -d --rm --name "$CONTAINER" \
  -p "127.0.0.1:$PORT:8080" \
  -e "DOCQL_INTERNAL_KEY=$INTERNAL_KEY" \
  ${ENV_FLAGS[@]+"${ENV_FLAGS[@]}"} \
  "$PIN" >/dev/null

# Poll /readyz for up to 30 s (a tampered kit fails the image boot, so this gates that too).
READY=0
for _ in $(seq 1 30); do
  if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/readyz" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 1
done
if [ "$READY" != 1 ]; then
  echo "bump-stub: container never became ready on port $PORT" >&2
  exit 1
fi

# Fetch a served file. An HTTP error or an empty body counts as a mismatch.
serve() { # serve <url-path> <dest> <name>
  local path="$1" dest="$2" name="$3"
  mkdir -p "$(dirname "$dest")"
  if ! curl -fsS --max-time 30 "http://127.0.0.1:$PORT$path" -o "$dest" || [ ! -s "$dest" ]; then
    echo "CONTRACT_KIT_MISMATCH $name"
    exit 1
  fi
}

serve "/__stub/sdk-contract/manifest.json" "$TMP/kit/manifest.json" "manifest.json"

NAMES=$(jq -r '.files | keys[]' "$TMP/kit/manifest.json")
while IFS= read -r name; do
  [ -n "$name" ] || continue
  serve "/__stub/sdk-contract/$name" "$TMP/kit/$name" "$name"
done <<< "$NAMES"

# Every served file must equal its own manifest entry (the image self-describes correctly).
while IFS=$'\t' read -r name want; do
  [ -n "$name" ] || continue
  got=$(sha256sum "$TMP/kit/$name" | cut -d' ' -f1)
  if [ "$got" != "$want" ]; then
    echo "CONTRACT_KIT_MISMATCH $name"
    exit 1
  fi
done < <(jq -r '.files | to_entries[] | "\(.key)\t\(.value)"' "$TMP/kit/manifest.json")

# The /__stub/sdk-cases route must serve the same bytes as the manifest's sdk-cases.json.
serve "/__stub/sdk-cases" "$TMP/cases-route" "sdk-cases.json"
ROUTE_SHA=$(sha256sum "$TMP/cases-route" | cut -d' ' -f1)
CASES_SHA=$(jq -r '.files["sdk-cases.json"]' "$TMP/kit/manifest.json")
if [ "$ROUTE_SHA" != "$CASES_SHA" ]; then
  echo "CONTRACT_KIT_MISMATCH sdk-cases.json"
  exit 1
fi

if [ "$MODE" = bump ]; then
  VERSION=$(jq -r '.version' "$TMP/kit/sdk-cases.json")
  rm -rf "$CONTRACT_DIR"
  mkdir -p "$CONTRACT_DIR"
  cp "$TMP/kit/manifest.json" "$CONTRACT_DIR/manifest.json"
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    mkdir -p "$CONTRACT_DIR/$(dirname "$name")"
    cp "$TMP/kit/$name" "$CONTRACT_DIR/$name"
  done <<< "$NAMES"
  printf '%s\n' "$PIN" > "$CONTRACT_DIR/stub-image"
  echo "bumped to $PIN (sdk-cases version $VERSION)"
  exit 0
fi

# --verify: every served file must equal the vendored copy, sha256 to sha256.
check() { # check <name> <served-file>
  # Two statements: a single `local` would expand $name before it is assigned.
  local name="$1" served="$2" got want
  local vendored="$CONTRACT_DIR/$name"
  if [ ! -f "$vendored" ]; then
    echo "CONTRACT_KIT_MISMATCH $name"
    exit 1
  fi
  got=$(sha256sum "$served" | cut -d' ' -f1)
  want=$(sha256sum "$vendored" | cut -d' ' -f1)
  if [ "$got" != "$want" ]; then
    echo "CONTRACT_KIT_MISMATCH $name"
    exit 1
  fi
}

COUNT=1 # manifest.json
check "manifest.json" "$TMP/kit/manifest.json"
while IFS= read -r name; do
  [ -n "$name" ] || continue
  check "$name" "$TMP/kit/$name"
  COUNT=$((COUNT + 1))
done <<< "$NAMES"
check "sdk-cases.json" "$TMP/cases-route"

# Any vendored file the served manifest does not know is drift (stub-image and
# manifest.json excepted — stub-image is the local pin record, not a kit file).
while IFS= read -r f; do
  [ -n "$f" ] || continue
  case "$f" in
    stub-image | manifest.json) continue ;;
  esac
  if ! grep -qxF "$f" <<< "$NAMES"; then
    echo "CONTRACT_KIT_MISMATCH $f (vendored, not in the served manifest)"
    exit 1
  fi
done < <(cd "$CONTRACT_DIR" && find . -type f | sed 's|^\./||')

echo "CONTRACT_KIT_OK $COUNT files"
