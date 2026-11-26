#!/usr/bin/env bash
# generated.sh generate|check: cli's generated files, written by the generator
# the SDK bundle carries (sdk's codegen, at the commit upstream.lock.json locks) from the
# Protocol this checkout pins. generate writes them into the checkout; check
# writes them into a copy and fails naming each file that differs from the
# committed one, and each committed file that says it is generated and that
# no run writes.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"
source "$ROOT/build-tools/container-runtime.sh"
[[ $# -eq 1 && ( "$1" == generate || "$1" == check ) ]] || { echo "Usage: $0 generate|check" >&2; exit 2; }
: "${GO_IMAGE:?Set GO_IMAGE to a Go container image.}"
bundle="$(bash "$ROOT/build-tools/prepare-sdk-artifacts.sh" codegen)"
RUNTIME="$(detect_container_runtime)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

if [[ "$1" == generate ]]; then
  target="$ROOT"
else
  target="$work/cli"
  mkdir "$target"
  tar -C "$ROOT" --exclude=./.git --exclude=./dist --exclude=./.local --exclude=node_modules -cf - . \
    | tar -C "$target" -xf -
fi

# The bundle's copy has lost its mode, so the container installs it to run.
"$RUNTIME" run --rm --volume "$target:/cli" --volume "$bundle/codegen:/codegen:ro" "$GO_IMAGE" sh -euc '
  install -m 0755 /codegen/xynolog-codegen /tmp/xynolog-codegen
  /tmp/xynolog-codegen generate cli /cli' | tee "$work/written"

if [[ "$1" == check ]]; then
  [[ -s "$work/written" ]]
  awk '{print $2}' "$work/written" | sort > "$work/paths"
  drifted=0
  while read -r path; do
    if [[ ! -f "$ROOT/$path" ]]; then
      echo "absent $path"
      drifted=$((drifted + 1))
    elif ! cmp -s "$target/$path" "$ROOT/$path"; then
      echo "DRIFT  $path"
      drifted=$((drifted + 1))
    fi
  done < "$work/paths"
  git -C "$ROOT" grep -l -E '^// Code generated .* DO NOT EDIT\.$' -- '*.go' | sort > "$work/banners"
  while read -r path; do
    echo "unwritten $path: it says it is generated, and no run writes it"
    drifted=$((drifted + 1))
  done < <(comm -23 "$work/banners" "$work/paths")
  if ((drifted)); then
    echo "$drifted generated files differ from what the generator writes: run make generate" >&2
    exit 1
  fi
fi
