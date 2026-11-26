#!/usr/bin/env bash
# release-version.sh <tag>: the CLI release a tag names, or a refusal. A CLI
# release is YY.M.N, tagged vYY.M.N (versioning R14): the year and month it is
# released in, and N counting from 0 within that month, so v26.11.0 is the
# first release of November 2026.
set -euo pipefail
[[ $# -eq 1 ]] || { echo "Usage: $0 <tag>" >&2; exit 2; }
[[ "$1" =~ ^v([0-9]{2}\.(1[0-2]|[1-9])\.(0|[1-9][0-9]*))$ ]] || { echo "the tag $1 is not vYY.M.N" >&2; exit 1; }
echo "${BASH_REMATCH[1]}"
