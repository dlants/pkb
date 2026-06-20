#!/usr/bin/env bash
# Build the pkb binary and install the git hooks from hooks/ into .git/hooks/.
# Run this once after cloning (and again after pulling hook changes).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

echo "building pkb..."
go build -o pkb ./cmd/pkb

echo "installing hooks..."
for src in hooks/*; do
  name="$(basename "$src")"
  dest=".git/hooks/$name"
  cp "$src" "$dest"
  chmod +x "$dest"
  echo "  installed $name"
done

echo "done."
