#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

# Delegate to the upstream `test` target so this stays correct as the module
# layout of the go workspace changes (`./...` only matches the root module, so
# every extra module has to be listed explicitly). GOFLAGS adds the flags we
# want in CI without carrying a patch on the upstream Makefile.
export GOFLAGS="${GOFLAGS:-} -count=1 -v"
exec make -C "$(dirname "${BASH_SOURCE[0]}")/.." test
