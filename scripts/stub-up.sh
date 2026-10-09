#!/usr/bin/env bash
# scripts/stub-up.sh — boot the digest-pinned docql-stub and run a command against it.
#
# Usage:
#   scripts/stub-up.sh --run <command> [args...]   # DOCQL_STUB_URL is exported to <command>
#
# STUB_PORT (default 18081) is the loopback port the stub container is published on.
#
# The pin is read from contract/stub-image and regex-validated before docker pull,
# so only the reviewed image digest ever runs. The stubEnv recipe (extra keys, test
# knobs, upload cap) comes from the vendored contract/sdk-cases.json, the same file
# the case runner reads, so a local run and CI drive the identical stub.
#
# The DOCQL_INTERNAL_KEY is generated fresh for this run and never printed. The
# container is always removed (EXIT trap) and the command's exit status is returned.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
CONTRACT_DIR="$ROOT/contract"
PORT=${STUB_PORT:-18081}
CONTAINER="docql-sdk-stub-$$"

PIN_RE='^ghcr\.io/finoniq/docql-stub:sha-[0-9a-f]{7}@sha256:[0-9a-f]{64}$'
usage() {
  echo "usage: scripts/stub-up.sh --run <command> [args...]" >&2
  echo "  exports DOCQL_STUB_URL=http://127.0.0.1:<STUB_PORT (default 18081)> to <command>" >&2
  exit 2
}

[ "$#" -ge 2 ] || usage
[ "$1" = "--run" ] || usage
shift

if [ ! -f "$CONTRACT_DIR/stub-image" ]; then
  echo "contract/stub-image is missing — the pinned stub digest is not vendored" >&2
  exit 2
fi
PIN=$(cat "$CONTRACT_DIR/stub-image")
if ! [[ "$PIN" =~ $PIN_RE ]]; then
  echo "invalid pin: $PIN" >&2
  echo "expected: ghcr.io/finoniq/docql-stub:sha-<7 hex>@sha256:<64 hex>" >&2
  exit 2
fi

# Fresh random key for this run only; never echoed.
INTERNAL_KEY=$(openssl rand -hex 24)

ENV_FLAGS=()
while IFS= read -r line; do
  [ -n "$line" ] || continue
  ENV_FLAGS+=(-e "$line")
done < <(jq -r '.stubEnv | to_entries[] | "\(.key)=\(.value)"' "$CONTRACT_DIR/sdk-cases.json")

trap 'docker rm -f "$CONTAINER" >/dev/null 2>&1 || true' EXIT

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
  echo "stub-up: container never became ready on port $PORT" >&2
  exit 1
fi

export DOCQL_STUB_URL="http://127.0.0.1:$PORT"
"$@"
