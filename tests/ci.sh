#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

shellcheck tests/*.sh
go test -race ./...
tests/e2e.sh
