#!/usr/bin/env bash
# scripts/run-readme.sh — run the README quickstart against the pinned stub.
#
# Extracts the single fenced Go block between the docql:quickstart markers of
# README.md, writes it as a package main program into a fresh temporary
# directory next to invoice.pdf (the golden kit's ascii-query document), and
# runs it there with `go run`, with DOCQL_API_KEY and DOCQL_API_URL pointing
# at the stub. CI and the release verify job run this script unchanged.
#
# Usage:
#   DOCQL_STUB_URL=http://127.0.0.1:18081 scripts/run-readme.sh
#   DOCQL_STUB_URL=... scripts/run-readme.sh --readme PATH
#   DOCQL_STUB_URL=... scripts/run-readme.sh --module-version VERSION
#
# Local mode (default) builds against this checkout with GOPROXY=off and a
# replace directive, proving the quickstart needs no third-party module and
# nothing from a registry. --module-version V instead resolves the module
# through proxy.golang.org with checksum verification on, prints
# RESOLVED_VERSION=<v>, checks the resolved version against V, requires the
# module's own go.mod to carry no require/toolchain line (NO_REQUIRE_OK) and
# then runs the program — the release dry run for the README quickstart.
#
# Prints README_QUICKSTART_OK on success and README_QUICKSTART_MISSING when a
# marker or the fenced block is unusable. Exits non-zero on any failure.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

README="$ROOT/README.md"
MODULE_VERSION=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --readme)
      [ "$#" -ge 2 ] || { echo "--readme needs a path" >&2; exit 2; }
      README=$2
      shift 2
      ;;
    --module-version)
      [ "$#" -ge 2 ] || { echo "--module-version needs a version" >&2; exit 2; }
      MODULE_VERSION=$2
      shift 2
      ;;
    *)
      echo "usage: scripts/run-readme.sh [--readme PATH] [--module-version VERSION]" >&2
      exit 2
      ;;
  esac
done

STUB_URL=${DOCQL_STUB_URL:-}
if [ -z "$STUB_URL" ]; then
  echo "DOCQL_STUB_URL is not set: boot the pinned stub (scripts/stub-up.sh)" >&2
  exit 1
fi

START='<!-- docql:quickstart:start -->'
END='<!-- docql:quickstart:end -->'

missing() {
  echo "README_QUICKSTART_MISSING" >&2
  exit 1
}

# Marker rules: each marker exactly once as a whole line, start before end.
starts=$(grep -cFx "$START" "$README" || true)
ends=$(grep -cFx "$END" "$README" || true)
[ "$starts" -eq 1 ] && [ "$ends" -eq 1 ] || missing
sline=$(grep -nFx "$START" "$README" | cut -d: -f1)
eline=$(grep -nFx "$END" "$README" | cut -d: -f1)
[ "$sline" -lt "$eline" ] || missing

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Fence rules: exactly one line equal to ```go and one line equal to ```
# strictly between the markers, open before close, block non-empty.
awk -v s="$sline" -v e="$eline" 'NR > s && NR < e && $0 == "```go" { print NR }' "$README" > "$TMP/opens"
awk -v s="$sline" -v e="$eline" 'NR > s && NR < e && $0 == "```" { print NR }' "$README" > "$TMP/closes"
[ "$(wc -l < "$TMP/opens")" -eq 1 ] || missing
[ "$(wc -l < "$TMP/closes")" -eq 1 ] || missing
open=$(head -n 1 "$TMP/opens")
close=$(head -n 1 "$TMP/closes")
[ "$open" -lt "$close" ] || missing
[ "$((close - open))" -gt 1 ] || missing

sed -n "$((open + 1)),$((close - 1))p" "$README" > "$TMP/main.go"

# invoice.pdf is the golden kit's ascii-query document; the key comes from the
# vendored case list, so the run needs no real credential.
jq -r '.cases[] | select(.id == "ascii-query") | .fileBase64' "$ROOT/contract/golden.json" | base64 -d > "$TMP/invoice.pdf"
API_KEY=$(jq -r '.keys.valid' "$ROOT/contract/sdk-cases.json")

export GOFLAGS=-mod=mod
if [ -n "$MODULE_VERSION" ]; then
  # Module mode: fetch through the real proxy with checksum verification on
  # and no private-module bypass, so what is executed is what the registry
  # serves. The retry loop absorbs proxy lag behind a fresh tag or push.
  cat > "$TMP/go.mod" <<EOF
module quickstart

go 1.26
EOF
  export GOPROXY=https://proxy.golang.org
  export GOSUMDB=sum.golang.org
  export GOPRIVATE=
  export GONOSUMDB=
  export GONOPROXY=
  ok=0
  for _ in $(seq 1 10); do
    if (cd "$TMP" && go get github.com/finoniq/docql-go@"$MODULE_VERSION" >/dev/null 2>&1); then
      ok=1
      break
    fi
    sleep 30
  done
  if [ "$ok" -ne 1 ]; then
    echo "go get github.com/finoniq/docql-go@$MODULE_VERSION failed after 10 tries:" >&2
    (cd "$TMP" && go get github.com/finoniq/docql-go@"$MODULE_VERSION") || true
    exit 1
  fi
  resolved=$(cd "$TMP" && go list -m github.com/finoniq/docql-go)
  echo "RESOLVED_VERSION=$resolved"
  case "$MODULE_VERSION" in
    v[0-9]*.[0-9]*.[0-9]*)
      [ "$resolved" = "$MODULE_VERSION" ] || { echo "resolved $resolved is not the tag $MODULE_VERSION" >&2; exit 1; }
      ;;
    *)
      want=$(printf '%s' "$MODULE_VERSION" | cut -c1-12 | tr 'A-F' 'a-f')
      case "$resolved" in
        *-"$want") ;;
        *) echo "resolved $resolved does not carry the commit $MODULE_VERSION" >&2; exit 1 ;;
      esac
      ;;
  esac
  modgomod=$(cd "$TMP" && go mod download -json "github.com/finoniq/docql-go@$resolved" | jq -r '.GoMod')
  if grep -qE '^[[:space:]]*(require|toolchain)[[:space:]]' "$modgomod"; then
    echo "the module's own go.mod carries a require or toolchain line:" >&2
    cat "$modgomod" >&2
    exit 1
  fi
  echo "NO_REQUIRE_OK"
else
  # Local mode: build against this checkout, offline.
  cat > "$TMP/go.mod" <<EOF
module quickstart

go 1.26

require github.com/finoniq/docql-go v0.0.0

replace github.com/finoniq/docql-go => $ROOT
EOF
  export GOPROXY=off
fi

export DOCQL_API_KEY="$API_KEY"
export DOCQL_API_URL="$STUB_URL"

set +e
out=$(cd "$TMP" && timeout 120 go run . 2>"$TMP/quickstart.stderr")
status=$?
set -e
if [ "$status" -ne 0 ]; then
  cat "$TMP/quickstart.stderr" >&2
  echo "the README quickstart failed (exit $status)" >&2
  exit 1
fi
if [ -n "$out" ]; then
  printf '%s\n' "$out"
fi
echo "README_QUICKSTART_OK"
