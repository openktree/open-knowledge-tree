#!/usr/bin/env bash
# Asserts the pushed tag matches the expected service component.
# Guard used in release-<service>.yml workflows to short-circuit
# misfired workflow runs (e.g. a tag dispatched without a matching
# service). Exits non-zero on mismatch.
#
# Usage:
#   verify-tag.sh api    # tag must be api-v<semver>
#   verify-tag.sh registry
set -euo pipefail

service="${1:?usage: verify-tag.sh <service>}"
tag="${GITHUB_REF_NAME:-}"

case "$tag" in
    "${service}-v"*) exit 0 ;;
    *) echo "verify-tag: tag '${tag}' does not match service '${service}'" >&2; exit 1 ;;
esac