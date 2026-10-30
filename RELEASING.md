# Releasing

A release is an annotated tag on a commit on `main` whose CI passed, with
the next version. The Go module proxy serves it from here as
`go.origincloud.eu/sdk@<version>`:

```sh
git tag -a v0.3.0 -m v0.3.0 origin/main
git push origin v0.3.0
```

Then check that it resolves:

```sh
GOFLAGS=-mod=mod go list -m go.origincloud.eu/sdk@v0.3.0
```

Versions follow semantic versioning. Before v1, a minor version may change
the API incompatibly, as the preview APIs themselves may (a `v1beta1`
package). A version is never moved or deleted once released: the module proxy
and every `go.sum` that names it keep it.

The Terraform provider (origincloudhq/terraform-provider-origincloud) moves
to the newest release on its next daily regeneration.
