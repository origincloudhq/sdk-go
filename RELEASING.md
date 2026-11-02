# Releasing

A release is an annotated tag `v<version>` on a commit on `main` whose CI
passed, the version that commit sets in `Version` in `core/client.go`, which
the SDK sends in its User-Agent. The Go module proxy serves it from here as
`go.origincloud.eu/sdk@<version>`.

Versions follow semantic versioning. Before v1, a minor version may change
the API incompatibly, as the preview APIs themselves may (a `v1beta1`
package). A version is never moved or deleted once released: the module proxy
and every `go.sum` that names it keep it.

## The automatic path

1. **Regeneration.** When the API changes, `regenerate.yml` opens or refreshes
   the pull request from the branch `regenerate`, and sets the next version in
   it with `scripts/version.sh bump`: a minor version when `API_REVISION` moved,
   a patch for a change of the generator alone.
2. **Merge.** Once CI passes on that pull request's head commit,
   `merge-regeneration.yml` merges it, if every commit in it is the
   regeneration workflow's. A pull request someone added commits to waits for
   review.
3. **Release.** Once CI passes on `main`, `release.yml` tags the version if it
   has no tag yet (`scripts/version.sh unreleased`), waits until the module
   proxy serves it, and dispatches the Terraform provider's regeneration
   (origincloudhq/terraform-provider-origincloud), which moves it to the new
   release.

The merge and the dispatch use the regeneration App's token, whose ID is the
variable `REGENERATE_APP_ID` and whose key is the secret
`REGENERATE_APP_PRIVATE_KEY`. A merge with the workflow token would start no
CI on `main`, and so no release. Without the App, step 1 still opens the pull
request, and the rest is done by hand; the provider's daily regeneration
picks up the release.

## By hand

A change outside a regeneration is released by setting `Version` in the same
pull request: `scripts/version.sh bump minor` or `scripts/version.sh bump
patch`. `release.yml` tags it once the pull request is merged and CI passes on
`main`.

Without the workflow, tag the commit yourself, then check that it resolves:

```sh
git tag -a v0.3.0 -m v0.3.0 origin/main
git push origin v0.3.0
cd "$(mktemp -d)" && GOFLAGS=-mod=mod go list -m go.origincloud.eu/sdk@v0.3.0
```
