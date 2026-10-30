#!/usr/bin/env bash
#
# Generates the service packages (<service>/<version>) from the API
# definitions in origincloudhq/api at the commit API_REVISION names:
# protoc-gen-go writes the messages and this repository's plugin
# (internal/cmd/protoc-gen-go-origincloud) the clients, both built at the
# versions go.mod pins.
#
#   scripts/generate.sh           # check the checked-in code is current
#   scripts/generate.sh --write   # generate it again
#
# A package's directory is its path without the leading origincloud/, and
# its name that path without slashes: origincloud/pvs/v1beta1 is
# go.origincloud.eu/sdk/pvs/v1beta1, package pvsv1beta1, the name buf's
# managed mode gives it too.
#
# Needs go, git and network access; buf runs through `go run` at the version
# below.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT
readonly API_REPOSITORY="https://github.com/origincloudhq/api.git"
readonly MODULE="go.origincloud.eu/sdk"
readonly BUF="github.com/bufbuild/buf/cmd/buf@v1.73.0"

revision="$(tr -d '[:space:]' < "${ROOT}/API_REVISION")"
[[ "${revision}" =~ ^[0-9a-f]{40}$ ]] || { echo "API_REVISION must hold a full commit hash" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

git init --quiet "${tmp}/api"
git -C "${tmp}/api" fetch --quiet --depth 1 "${API_REPOSITORY}" "${revision}"
git -C "${tmp}/api" checkout --quiet FETCH_HEAD
(
  cd "${ROOT}"
  go build -o "${tmp}/bin/protoc-gen-go" google.golang.org/protobuf/cmd/protoc-gen-go
  go build -o "${tmp}/bin/protoc-gen-go-origincloud" ./internal/cmd/protoc-gen-go-origincloud
)

opts="\"module=${MODULE}\""
while read -r file; do
  dir="$(dirname "${file}")"
  rel="${dir#origincloud/}"
  opts+=",\"M${file}=${MODULE}/${rel};${rel//\//}\""
done < <(cd "${tmp}/api" && find origincloud -name '*.proto' | LC_ALL=C sort)

template="$(printf '{"version":"v2","plugins":[{"local":"%s","out":"%s","opt":[%s]},{"local":"%s","out":"%s","opt":[%s]}]}' \
  "${tmp}/bin/protoc-gen-go" "${tmp}/gen" "${opts}" \
  "${tmp}/bin/protoc-gen-go-origincloud" "${tmp}/gen" "${opts}")"
(cd "${ROOT}" && go run "${BUF}" generate "${tmp}/api" --template "${template}")

# The generated files, relative to the repository.
generated() { (cd "$1" && find . -name '*.pb.go' -not -path './internal/*' | LC_ALL=C sort); }

if [[ "${1:-}" == "--write" ]]; then
  generated "${ROOT}" | (cd "${ROOT}" && xargs -r rm -f)
  (cd "${tmp}/gen" && find . -name '*.pb.go' -exec cp --parents {} "${ROOT}" \;)
  echo "wrote $(generated "${ROOT}" | wc -l) files from origincloudhq/api@${revision:0:12}"
  exit 0
fi

failed=0
if ! diff <(generated "${ROOT}") <(generated "${tmp}/gen") >/dev/null; then
  failed=1
else
  while read -r file; do
    cmp -s "${ROOT}/${file}" "${tmp}/gen/${file}" || failed=1
  done < <(generated "${ROOT}")
fi
if [[ "${failed}" -eq 1 ]]; then
  echo "FAILED: the generated code isn't what origincloudhq/api@${revision:0:12} generates; run $0 --write"
  exit 1
fi
echo "ok - the generated code is current (origincloudhq/api@${revision:0:12})"
