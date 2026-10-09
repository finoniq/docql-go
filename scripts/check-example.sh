#!/usr/bin/env bash
# scripts/check-example.sh — keep ExampleClient_QueryDocument and the README
# quickstart telling the same story.
#
# Extracts the README quickstart block with the same rules run-readme.sh
# applies, takes the body of main() from it and the body of
# ExampleClient_QueryDocument from example_test.go, strips leading and
# trailing whitespace from every line, and compares. CI runs this, so the
# pkg.go.dev example can never drift from the README.
#
# Usage:
#   scripts/check-example.sh [--readme PATH] [--example PATH]
#
# Prints EXAMPLE_MATCHES_README (exit 0) on equality, EXAMPLE_DIFFERS with a
# unified diff (exit 1) on drift, and EXAMPLE_MISSING (exit 1) when the
# markers are unusable or either function is absent.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

README="$ROOT/README.md"
EXAMPLE="$ROOT/example_test.go"
while [ "$#" -gt 0 ]; do
  case "$1" in
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
      echo "usage: scripts/check-example.sh [--readme PATH] [--example PATH]" >&2
      exit 2
      ;;
  esac
done

START='<!-- docql:quickstart:start -->'
END='<!-- docql:quickstart:end -->'

missing() {
  echo "EXAMPLE_MISSING"
  exit 1
}

# Same marker and fence rules as run-readme.sh.
starts=$(grep -cFx "$START" "$README" || true)
ends=$(grep -cFx "$END" "$README" || true)
[ "$starts" -eq 1 ] && [ "$ends" -eq 1 ] || missing
sline=$(grep -nFx "$START" "$README" | cut -d: -f1)
eline=$(grep -nFx "$END" "$README" | cut -d: -f1)
[ "$sline" -lt "$eline" ] || missing

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

awk -v s="$sline" -v e="$eline" 'NR > s && NR < e && $0 == "```go" { print NR }' "$README" > "$TMP/opens"
awk -v s="$sline" -v e="$eline" 'NR > s && NR < e && $0 == "```" { print NR }' "$README" > "$TMP/closes"
[ "$(wc -l < "$TMP/opens")" -eq 1 ] || missing
[ "$(wc -l < "$TMP/closes")" -eq 1 ] || missing
open=$(head -n 1 "$TMP/opens")
close=$(head -n 1 "$TMP/closes")
[ "$open" -lt "$close" ] || missing
[ "$((close - open))" -gt 1 ] || missing

sed -n "$((open + 1)),$((close - 1))p" "$README" > "$TMP/block.go"

# body_of prints the lines strictly between "func NAME() {" and the next line
# equal to "}", whitespace-stripped per line; nothing when absent.
body_of() {
  awk -v fn="$1" '
    $0 == "func " fn "() {" { body = 1; next }
    body && $0 == "}" { exit }
    body { print }
  ' "$2" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}

body_of main "$TMP/block.go" > "$TMP/readme.body"
body_of ExampleClient_QueryDocument "$EXAMPLE" > "$TMP/example.body"

if [ ! -s "$TMP/readme.body" ] || [ ! -s "$TMP/example.body" ]; then
  missing
fi

if cmp -s "$TMP/readme.body" "$TMP/example.body"; then
  echo "EXAMPLE_MATCHES_README"
  exit 0
fi
echo "EXAMPLE_DIFFERS"
diff -u "$TMP/example.body" "$TMP/readme.body" || true
exit 1
