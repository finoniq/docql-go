#!/usr/bin/env bash
# scripts/check-example.sh — keep the pkg.go.dev examples and the README
# blocks telling the same story.
#
# For each README block (between <!-- docql:<name>:start --> and
# <!-- docql:<name>:end -->), extracts the fenced Go block with the same rules
# run-readme.sh applies, takes the body of main() from it and the body of the
# matching example function from example_test.go, strips leading and trailing
# whitespace from every line, and compares:
#
#   quickstart  ExampleClient_QueryDocument
#   jobs        ExampleClient_SubmitDocument
#
# CI runs this with no argument, which checks both blocks, so no pkg.go.dev
# example can drift from the README.
#
# Usage:
#   scripts/check-example.sh [--block quickstart|jobs] [--readme PATH] [--example PATH]
#
# The quickstart prints EXAMPLE_MATCHES_README on equality, EXAMPLE_DIFFERS
# with a unified diff on drift, and EXAMPLE_MISSING when the markers are
# unusable or either function is absent; the jobs block prints the same lines
# prefixed with JOBS_ (JOBS_EXAMPLE_MATCHES_README, ...). Exits 0 only when
# every checked block matches, 1 otherwise, 2 on a usage error.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

README="$ROOT/README.md"
EXAMPLE="$ROOT/example_test.go"
BLOCKS="quickstart jobs"
usage() {
  echo "usage: scripts/check-example.sh [--block quickstart|jobs] [--readme PATH] [--example PATH]" >&2
  exit 2
}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --block)
      [ "$#" -ge 2 ] || { echo "--block needs a name" >&2; exit 2; }
      case "$2" in
        quickstart|jobs) BLOCKS=$2 ;;
        *) usage ;;
      esac
      shift 2
      ;;
    --readme)
      [ "$#" -ge 2 ] || { echo "--readme needs a path" >&2; exit 2; }
      README=$2
      shift 2
      ;;
    --example)
      [ "$#" -ge 2 ] || { echo "--example needs a path" >&2; exit 2; }
      EXAMPLE=$2
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# body_of prints the lines strictly between "func NAME() {" and the next line
# equal to "}", whitespace-stripped per line; nothing when absent.
body_of() {
  awk -v fn="$1" '
    $0 == "func " fn "() {" { body = 1; next }
    body && $0 == "}" { exit }
    body { print }
  ' "$2" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}

# check_block NAME FUNC TAG compares README block NAME with FUNC and prints
# TAG-prefixed result lines. Returns 0 on a match, 1 otherwise.
check_block() {
  local name=$1 fn=$2 tag=$3
  local start="<!-- docql:$name:start -->" end="<!-- docql:$name:end -->"
  local dir="$TMP/$name"
  mkdir -p "$dir"

  # Same marker and fence rules as run-readme.sh.
  local starts ends sline eline open close
  starts=$(grep -cFx "$start" "$README" || true)
  ends=$(grep -cFx "$end" "$README" || true)
  if [ "$starts" -ne 1 ] || [ "$ends" -ne 1 ]; then
    echo "${tag}EXAMPLE_MISSING"
    return 1
  fi
  sline=$(grep -nFx "$start" "$README" | cut -d: -f1)
  eline=$(grep -nFx "$end" "$README" | cut -d: -f1)
  if [ "$sline" -ge "$eline" ]; then
    echo "${tag}EXAMPLE_MISSING"
    return 1
  fi
  awk -v s="$sline" -v e="$eline" 'NR > s && NR < e && $0 == "```go" { print NR }' "$README" > "$dir/opens"
  awk -v s="$sline" -v e="$eline" 'NR > s && NR < e && $0 == "```" { print NR }' "$README" > "$dir/closes"
  if [ "$(wc -l < "$dir/opens")" -ne 1 ] || [ "$(wc -l < "$dir/closes")" -ne 1 ]; then
    echo "${tag}EXAMPLE_MISSING"
    return 1
  fi
  open=$(head -n 1 "$dir/opens")
  close=$(head -n 1 "$dir/closes")
  if [ "$open" -ge "$close" ] || [ "$((close - open))" -le 1 ]; then
    echo "${tag}EXAMPLE_MISSING"
    return 1
  fi

  sed -n "$((open + 1)),$((close - 1))p" "$README" > "$dir/block.go"
  body_of main "$dir/block.go" > "$dir/readme.body"
  body_of "$fn" "$EXAMPLE" > "$dir/example.body"
  if [ ! -s "$dir/readme.body" ] || [ ! -s "$dir/example.body" ]; then
    echo "${tag}EXAMPLE_MISSING"
    return 1
  fi

  if cmp -s "$dir/readme.body" "$dir/example.body"; then
    echo "${tag}EXAMPLE_MATCHES_README"
    return 0
  fi
  echo "${tag}EXAMPLE_DIFFERS"
  diff -u "$dir/example.body" "$dir/readme.body" || true
  return 1
}

rc=0
for block in $BLOCKS; do
  case "$block" in
    quickstart) check_block quickstart ExampleClient_QueryDocument "" || rc=1 ;;
    jobs) check_block jobs ExampleClient_SubmitDocument "JOBS_" || rc=1 ;;
  esac
done
exit "$rc"
