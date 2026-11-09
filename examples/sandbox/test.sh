#!/bin/sh
# Pretend test suite: fails if any file under src/ contains the word BUG.
set -e
echo "Running tests..."
found=$(grep -rn "BUG" src || true)
if [ -n "$found" ]; then
  echo "$found"
  echo "FAIL: found BUG in the code above"
  exit 1
fi
for f in src/*.txt; do echo "ok  $f"; done
echo "All tests passed."
