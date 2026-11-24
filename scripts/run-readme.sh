#!/usr/bin/env bash
# scripts/run-readme.sh — run a README example against the pinned stub.
#
# Extracts the single fenced Go block between the docql:<block> markers of
# README.md (--block quickstart, the default, --block jobs, --block
# spreadsheet or --block webhook), writes it as a package main program into a
# fresh temporary directory next to invoice.pdf (the golden kit's ascii-query
# document) and sales.csv (a copy of the kit's contract/sheets/plain.csv), and
# runs it there with `go run`, with DOCQL_API_KEY and DOCQL_API_URL pointing at
# the stub. CI and the release verify job run this script unchanged.
#
# --sheet-name NAME (spreadsheet block only, WR-04) copies plain.csv as NAME
# instead and points both "sales.csv" literals of the block at it, so a stub
# fixture name such as toolarge-cells-558-557.csv runs the refusal branch; the
# tag becomes README_SPREADSHEET_REFUSAL. --expect-line TEXT requires that
# exact line in the example's output before the OK tag prints.
#
# The webhook block (D-23) is a receiver that serves until it is stopped. The
# script builds it first, starts it on a free loopback port (18300..18399)
# with a whsec_ secret made for this run (never printed), POSTs one delivery
# signed at run time with an openssl HMAC and expects 2xx plus the line
# `event job.succeeded <id>`, then POSTs the same headers with one body byte
# changed and expects 400 and no second line. It needs openssl, od and curl.
#
# Usage:
#   DOCQL_STUB_URL=http://127.0.0.1:18081 scripts/run-readme.sh
#   DOCQL_STUB_URL=... scripts/run-readme.sh --block jobs
#   DOCQL_STUB_URL=... scripts/run-readme.sh --block spreadsheet
#   DOCQL_STUB_URL=... scripts/run-readme.sh --block spreadsheet --sheet-name NAME --expect-line TEXT
#   DOCQL_STUB_URL=... scripts/run-readme.sh --block webhook
#   DOCQL_STUB_URL=... scripts/run-readme.sh --readme PATH
#   DOCQL_STUB_URL=... scripts/run-readme.sh --module-version VERSION [--block jobs|spreadsheet|webhook]
#
# Local mode (default) builds against this checkout with GOPROXY=off and a
# replace directive, proving the quickstart needs no third-party module and
# nothing from a registry. --module-version V instead resolves the module
# through proxy.golang.org with checksum verification on, prints
# RESOLVED_VERSION=<v>, checks the resolved version against V, requires the
# module's own go.mod to carry no require/toolchain line (NO_REQUIRE_OK) and
# then runs the program — the release dry run for the README examples.
#
# Prints README_<BLOCK>_OK (README_QUICKSTART_OK, README_JOBS_OK,
# README_SPREADSHEET_OK, README_WEBHOOK_OK, README_SPREADSHEET_REFUSAL_OK with
# --sheet-name) on success and README_<BLOCK>_MISSING when a marker or the
# fenced block is unusable; README_WEBHOOK_FAIL <problem> when the receiver
# misbehaves. Exits non-zero on any failure.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

README="$ROOT/README.md"
MODULE_VERSION=""
BLOCK=quickstart
SHEET_NAME=""
EXPECT_LINE=""
HAVE_EXPECT_LINE=0
usage() {
  echo "usage: scripts/run-readme.sh [--block quickstart|jobs|spreadsheet|webhook] [--sheet-name NAME] [--expect-line TEXT] [--readme PATH] [--module-version VERSION]" >&2
  exit 2
}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --block)
      [ "$#" -ge 2 ] || { echo "--block needs a name" >&2; exit 2; }
      case "$2" in
        quickstart|jobs|spreadsheet|webhook) BLOCK=$2 ;;
        *) usage ;;
      esac
      shift 2
      ;;
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
    --sheet-name)
      [ "$#" -ge 2 ] || { echo "--sheet-name needs a file name" >&2; exit 2; }
      SHEET_NAME=$2
      shift 2
      ;;
    --expect-line)
      [ "$#" -ge 2 ] || { echo "--expect-line needs a line" >&2; exit 2; }
      EXPECT_LINE=$2
      HAVE_EXPECT_LINE=1
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

START="<!-- docql:$BLOCK:start -->"
END="<!-- docql:$BLOCK:end -->"
TAG=$(printf '%s' "$BLOCK" | tr 'a-z' 'A-Z')
CSV_NAME=sales.csv
if [ -n "$SHEET_NAME" ]; then
  [ "$BLOCK" = spreadsheet ] || { echo "--sheet-name needs --block spreadsheet" >&2; exit 2; }
  # A plain file name only: it is written into the temporary directory and
  # pasted into a Go string literal.
  if ! [[ "$SHEET_NAME" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    echo "--sheet-name must be a plain file name ending in .csv" >&2
    exit 2
  fi
  case "$(printf '%s' "$SHEET_NAME" | tr 'A-Z' 'a-z')" in
    *.csv) ;;
    *) echo "--sheet-name must be a plain file name ending in .csv" >&2; exit 2 ;;
  esac
  CSV_NAME=$SHEET_NAME
  TAG=SPREADSHEET_REFUSAL
fi

STUB_URL=${DOCQL_STUB_URL:-}
if [ -z "$STUB_URL" ]; then
  echo "DOCQL_STUB_URL is not set: boot the pinned stub (scripts/stub-up.sh)" >&2
  exit 1
fi

missing() {
  echo "README_${TAG}_MISSING" >&2
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
RECEIVER_PID=""
cleanup() {
  if [ -n "$RECEIVER_PID" ]; then
    kill "$RECEIVER_PID" 2>/dev/null || true
    wait "$RECEIVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

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
if [ -n "$SHEET_NAME" ]; then
  if [ "$(grep -oF '"sales.csv"' "$TMP/main.go" | wc -l)" -ne 2 ]; then
    echo "README_${TAG}_UNEXPECTED_BLOCK: want exactly two \"sales.csv\" literals"
    exit 1
  fi
  sed "s/\"sales\\.csv\"/\"$SHEET_NAME\"/g" "$TMP/main.go" >"$TMP/main.go.new"
  mv "$TMP/main.go.new" "$TMP/main.go"
fi

# invoice.pdf is the golden kit's ascii-query document; the key comes from the
# vendored case list, so the run needs no real credential.
jq -r '.cases[] | select(.id == "ascii-query") | .fileBase64' "$ROOT/contract/golden.json" | base64 -d > "$TMP/invoice.pdf"
API_KEY=$(jq -r '.keys.valid' "$ROOT/contract/sdk-cases.json")
# sales.csv (or the --sheet-name file) is the sheet kit's plain.csv, copied
# into this run's own temporary directory so two runs never share the file.
cp "$ROOT/contract/sheets/plain.csv" "$TMP/$CSV_NAME"

export GOFLAGS=-mod=mod
if [ -n "$MODULE_VERSION" ]; then
  # Module mode: fetch through the real proxy with checksum verification on
  # and no private-module bypass, so what is executed is what the registry
  # serves. The retry loop absorbs proxy lag behind a fresh tag or push.
  cat > "$TMP/go.mod" <<EOF
module $BLOCK

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
  resolved=$(cd "$TMP" && go list -m -f '{{.Version}}' github.com/finoniq/docql-go)
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
module $BLOCK

go 1.26

require github.com/finoniq/docql-go v0.0.0

replace github.com/finoniq/docql-go => $ROOT
EOF
  export GOPROXY=off
fi

export DOCQL_API_KEY="$API_KEY"
export DOCQL_API_URL="$STUB_URL"

# The webhook receiver (D-23): build it, serve it in the background, send one
# delivery signed now and one with a changed body byte.
WEBHOOK_EVENT_ID=5f0c3a1e-45a1-4d1e-9b00-000000000451
webhook_fail() {
  cat "$TMP/receiver.out" "$TMP/receiver.err" >&2 2>/dev/null || true
  echo "README_WEBHOOK_FAIL $1"
  exit 1
}
webhook_post() { # webhook_post <body file>: prints the HTTP status, 000 when no answer
  curl -q -sS --noproxy '*' --max-time 10 -o /dev/null -w '%{http_code}' \
    -H 'content-type: application/json' \
    -H "webhook-id: $WEBHOOK_EVENT_ID" \
    -H "webhook-timestamp: $ts" \
    -H "webhook-signature: v1,$sig" \
    --data-binary "@$1" "http://127.0.0.1:$port/" 2>>"$TMP/curl.err" || true
}
run_webhook() {
  local secret p ready hex sent_at body tampered code expected
  set +e
  (cd "$TMP" && timeout 120 go build -o "$TMP/receiver" .) 2>"$TMP/build.stderr"
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    cat "$TMP/build.stderr" >&2
    echo "the README webhook example did not build (exit $status)" >&2
    exit 1
  fi

  # A secret in the console's text format, made for this run and never printed.
  secret="whsec_$(openssl rand 32 | openssl base64 -A)"
  port=""
  for p in $(seq 18300 18399); do
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
      port=$p
      break
    fi
  done
  [ -n "$port" ] || webhook_fail "no free loopback port in 18300..18399"
  : >"$TMP/receiver.out"
  (cd "$TMP" && export DOCQL_WEBHOOK_SECRET="$secret" PORT="$port" && exec "$TMP/receiver") \
    >"$TMP/receiver.out" 2>"$TMP/receiver.err" &
  RECEIVER_PID=$!
  ready=0
  for _ in $(seq 1 60); do
    kill -0 "$RECEIVER_PID" 2>/dev/null || webhook_fail "the receiver exited before it listened"
    if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
      ready=1
      break
    fi
    sleep 0.5
  done
  [ "$ready" -eq 1 ] || webhook_fail "the receiver did not listen on 127.0.0.1:$port within 30 s"

  # A job.succeeded body in the frozen shape, signed now: HMAC-SHA256 over
  # "<id>.<timestamp>.<body>" with the decoded key (hexkey, as the key is binary).
  ts=$(date +%s)
  sent_at=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
  body='{"id":"'"$WEBHOOK_EVENT_ID"'","type":"job.succeeded","timestamp":"'"$sent_at"'","data":{"job_id":"job_readme_7Hk2mQ9x","status":"succeeded"}}'
  tampered=${body/'"status":"succeeded"'/'"status":"succeedec"'}
  [ "$tampered" != "$body" ] || webhook_fail "could not change a byte of the delivery"
  hex=$(printf '%s' "${secret#whsec_}" | openssl base64 -d -A | od -An -v -tx1 | tr -d ' \n')
  [ "${#hex}" -eq 64 ] || webhook_fail "the run-time key is not 32 bytes"
  sig=$(printf '%s' "$WEBHOOK_EVENT_ID.$ts.$body" | openssl dgst -sha256 -mac HMAC -macopt "hexkey:$hex" -binary | openssl base64 -A)
  printf '%s' "$body" >"$TMP/delivery.json"
  printf '%s' "$tampered" >"$TMP/tampered.json"

  expected="event job.succeeded $WEBHOOK_EVENT_ID"
  code=$(webhook_post "$TMP/delivery.json")
  case "$code" in
    2??) ;;
    *) webhook_fail "the signed delivery got HTTP $code" ;;
  esac
  for _ in $(seq 1 20); do
    grep -qxF "$expected" "$TMP/receiver.out" && break
    sleep 0.5
  done
  grep -qxF "$expected" "$TMP/receiver.out" || webhook_fail "no line '$expected' for the signed delivery"

  code=$(webhook_post "$TMP/tampered.json")
  [ "$code" = 400 ] || webhook_fail "the tampered delivery got HTTP $code"
  sleep 0.5
  [ "$(wc -l <"$TMP/receiver.out")" -eq 1 ] || webhook_fail "the receiver printed more than the one event line"
  out=$(cat "$TMP/receiver.out")
}

if [ "$BLOCK" = webhook ]; then
  run_webhook
else
  set +e
  out=$(cd "$TMP" && timeout 120 go run . 2>"$TMP/$BLOCK.stderr")
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    cat "$TMP/$BLOCK.stderr" >&2
    if [ "$BLOCK" = quickstart ]; then
      echo "the README quickstart failed (exit $status)" >&2
    else
      echo "the README $BLOCK example failed (exit $status)" >&2
    fi
    exit 1
  fi
fi
if [ -n "$out" ]; then
  printf '%s\n' "$out"
fi
if [ "$HAVE_EXPECT_LINE" -eq 1 ] && ! printf '%s\n' "$out" | grep -qxF -- "$EXPECT_LINE"; then
  echo "README_${TAG}_EXPECTED_LINE_MISSING $EXPECT_LINE"
  exit 1
fi
echo "README_${TAG}_OK"
