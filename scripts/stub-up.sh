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
# knobs, upload cap, job knobs) comes from the vendored contract/job-cases.json, the
# superset of the contract/sdk-cases.json recipe; the script exits 2 if the two
# recipes drift apart, so a local run and CI drive the identical stub.
#
# DOCQL_STUB_IMAGE_OVERRIDE=<local image ref> (local only) runs an image that is
# already present on this machine instead of the pin: it is refused when CI or
# GITHUB_ACTIONS is set, never pulled, must pass `docker image inspect`, and the
# ref plus its image id are printed to stderr as `STUB_IMAGE_OVERRIDE <ref> <id>`.
# It never changes contract/stub-image; the pin moves only to a published GHCR digest.
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
OVERRIDE_RE='^[A-Za-z0-9][A-Za-z0-9._/:@-]*$'
usage() {
  echo "usage: scripts/stub-up.sh --run <command> [args...]" >&2
  echo "  exports DOCQL_STUB_URL=http://127.0.0.1:<STUB_PORT (default 18081)> to <command>" >&2
  exit 2
}

[ "$#" -ge 2 ] || usage
[ "$1" = "--run" ] || usage
shift

OVERRIDE=${DOCQL_STUB_IMAGE_OVERRIDE:-}
if [ -n "$OVERRIDE" ]; then
  if [ -n "${CI:-}" ] || [ -n "${GITHUB_ACTIONS:-}" ]; then
    echo "DOCQL_STUB_IMAGE_OVERRIDE is local-only; refused under CI" >&2
    exit 2
  fi
  if ! [[ "$OVERRIDE" =~ $OVERRIDE_RE ]]; then
    echo "invalid DOCQL_STUB_IMAGE_OVERRIDE: $OVERRIDE" >&2
    exit 2
  fi
  if ! IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$OVERRIDE" 2>/dev/null); then
    echo "override image not found locally: $OVERRIDE" >&2
    exit 2
  fi
  echo "STUB_IMAGE_OVERRIDE $OVERRIDE $IMAGE_ID" >&2
  IMAGE=$OVERRIDE
else
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
  IMAGE=$PIN
fi

# The job-cases.json recipe must carry every sdk-cases.json key: the same value, or
# for a comma list (STUB_EXTRA_KEYS) every one of its entries.
if ! jq -e -n --slurpfile sdk "$CONTRACT_DIR/sdk-cases.json" --slurpfile job "$CONTRACT_DIR/job-cases.json" '
  ($job[0].stubEnv) as $j
  | all($sdk[0].stubEnv | to_entries[];
      ($j[.key]) as $v | $v != null and ((.value | split(",")) - ($v | split(",")) | length) == 0)
' >/dev/null; then
  echo "stubEnv drift between sdk-cases.json and job-cases.json" >&2
  exit 2
fi

# Fresh random key for this run only; never echoed.
INTERNAL_KEY=$(openssl rand -hex 24)

ENV_FLAGS=()
while IFS= read -r line; do
  [ -n "$line" ] || continue
  ENV_FLAGS+=(-e "$line")
done < <(jq -r '.stubEnv | to_entries[] | "\(.key)=\(.value)"' "$CONTRACT_DIR/job-cases.json")

trap 'docker rm -f "$CONTAINER" >/dev/null 2>&1 || true' EXIT

if [ -z "$OVERRIDE" ]; then
  docker pull "$IMAGE" >/dev/null
fi

docker run -d --rm --name "$CONTAINER" \
  -p "127.0.0.1:$PORT:8080" \
  -e "DOCQL_INTERNAL_KEY=$INTERNAL_KEY" \
  ${ENV_FLAGS[@]+"${ENV_FLAGS[@]}"} \
  "$IMAGE" >/dev/null

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
