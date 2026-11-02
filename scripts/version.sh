#!/usr/bin/env bash
#
# The SDK's version: `Version` in core/client.go, which the SDK sends in its
# User-Agent and which a release tags (RELEASING.md).
#
#   scripts/version.sh current          # print it
#   scripts/version.sh bump minor|patch # set it to the next version
#   scripts/version.sh unreleased       # exit 0 if it has no tag yet and is
#                                       # newer than every tag, 1 otherwise
#
# `bump` sets it to the next version after the newest tag: a minor version
# when the API moved (before v1 a minor version may change the API
# incompatibly, as the preview APIs may), a patch otherwise. A version that
# is already newer than that, a bump not released yet, stays. The
# regeneration workflow bumps it in its pull request, and the release
# workflow tags it once that is merged.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT
readonly FILE="${ROOT}/core/client.go"
readonly PATTERN='^const Version = "([0-9]+\.[0-9]+\.[0-9]+)"$'

current() {
  local line
  line="$(grep -E "${PATTERN}" "${FILE}")" || { echo "no Version constant in ${FILE}" >&2; exit 1; }
  [[ "${line}" =~ ${PATTERN} ]]
  echo "${BASH_REMATCH[1]}"
}

# The newest release, without its `v`; 0.0.0 before the first.
newest_tag() {
  local tag
  tag="$(git -C "${ROOT}" tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n 1)"
  if [[ -n "${tag}" ]]; then
    echo "${tag#v}"
  else
    echo "0.0.0"
  fi
}

# Whether version $1 is newer than $2.
newer() {
  [[ "$1" != "$2" && "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n 1)" == "$1" ]]
}

case "${1:-}" in
  current)
    current
    ;;
  bump)
    kind="${2:-}"
    IFS=. read -r major minor patch <<< "$(newest_tag)"
    case "${kind}" in
      minor) next="${major}.$((minor + 1)).0" ;;
      patch) next="${major}.${minor}.$((patch + 1))" ;;
      *) echo "usage: $0 bump minor|patch" >&2; exit 2 ;;
    esac
    now="$(current)"
    if newer "${next}" "${now}"; then
      sed -i -E "s/^const Version = \"[0-9.]+\"$/const Version = \"${next}\"/" "${FILE}"
      echo "${next}"
    else
      echo "${now}"
    fi
    ;;
  unreleased)
    now="$(current)"
    newer "${now}" "$(newest_tag)"
    ;;
  *)
    echo "usage: $0 current | bump minor|patch | unreleased" >&2
    exit 2
    ;;
esac
