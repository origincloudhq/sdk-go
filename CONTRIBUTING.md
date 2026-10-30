# Contributing

Thank you for helping improve Origin Cloud's SDK for Go.

## Issues

Open an issue for anything that is wrong, unclear or missing: a method that
behaves differently from its description, an option you need, documentation that
doesn't explain enough. Say which package and method it is about, and include
the request and response where you can, with tokens and account details
removed.

To report a security vulnerability, don't open an issue: use **Report a
vulnerability** on this repository's Security tab, which reaches us privately.

## Pull requests

Pull requests are reviewed and merged here. For anything larger than a small
fix, open an issue first, so that we can agree on the change before you spend
time on it. CI must pass: the checks below, run on every pull request.

## How the SDK is built

The service packages (`<service>/<version>`) are generated from the API
definitions in [origincloudhq/api](https://github.com/origincloudhq/api), at
the commit `API_REVISION` names: protoc-gen-go writes the messages, and this
repository's plugin, `internal/cmd/protoc-gen-go-origincloud`, the clients.
`core` is written by hand. So a change to what a service offers starts in the
API, which is why it is requested by opening an issue rather than by editing
a generated package; a change to how every client behaves is a change to the
plugin or `core`.

The regeneration workflow (`.github/workflows/regenerate.yml`) moves the SDK
to the newest API daily, and opens a pull request when that changes anything.

## Checks

Go and network access are all you need; buf runs through `go run`.

```sh
scripts/generate.sh --write   # generate the service packages again; without --write, check them
scripts/update.sh             # move to the newest API, and generate
go test -race ./...           # the live tests skip without a zone
ORIGINCLOUD_LIVE_ZONE=projects/<project>/zones/<zone> \
  go test -count=1 -v ./internal/livetest   # against the live API
```

CI also checks formatting (`gofmt`), `go mod tidy`, `go vet` and the scripts.
Releases are described in [RELEASING.md](RELEASING.md).

## License

By contributing, you agree that your contribution is licensed under the
Apache License 2.0, like the rest of this repository.
