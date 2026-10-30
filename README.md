# Origin Cloud SDK for Go

The Go client for Origin Cloud's REST APIs. Its service packages are generated
from the API definitions in
[origincloudhq/api](https://github.com/origincloudhq/api); `core` is what they
share: credentials, endpoints, retries, errors, long-running operations and
read-your-writes.

```sh
go get go.origincloud.eu/sdk
```

| Package | API |
|---|---|
| `go.origincloud.eu/sdk/core` | The client every service package uses |
| `go.origincloud.eu/sdk/pvs/v1beta1` | Persistent volumes: volumes, snapshots, storage classes |
| `go.origincloud.eu/sdk/longrunning/v1beta1` | Operations, for resuming one by name |

## Example

```go
package main

import (
	"context"
	"log"

	"go.origincloud.eu/sdk/core"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func main() {
	ctx := context.Background()
	client, err := core.NewClient(ctx)
	if err != nil {
		log.Fatal(err)
	}
	volumes := pvsv1beta1.NewVolumeClient(client)

	op, err := volumes.CreateVolume(ctx, &pvsv1beta1.CreateVolumeRequest{
		Parent: "projects/my-project-123/zones/eu-central1-az1",
		Volume: &pvsv1beta1.Volume{SizeGib: 10},
	})
	if err != nil {
		log.Fatal(err)
	}
	volume, err := op.Wait(ctx)
	if err != nil {
		log.Fatal(err)
	}

	op, err = volumes.UpdateVolume(ctx, &pvsv1beta1.UpdateVolumeRequest{
		Volume:     &pvsv1beta1.Volume{Name: volume.GetName(), SizeGib: 20},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"size_gib"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	if volume, err = op.Wait(ctx); err != nil {
		log.Fatal(err)
	}
	log.Printf("%s has %d GiB", volume.GetName(), volume.GetSizeGib())
}
```

## Credentials

`core.NewClient` takes the first of:

1. `ORIGINCLOUD_ACCESS_TOKEN`: an access token, sent as it is.
2. `ORIGINCLOUD_CLIENT_ID` and `ORIGINCLOUD_CLIENT_SECRET`: a service account's
   credential, exchanged for access tokens as they expire. This is what
   automation should use.
3. The session the `origin` CLI stored at its last login (`origin id login`).

`core.WithCredentials` sets them explicitly instead.

## Behaviour

- **Endpoints.** Every service answers at `{service}.origincloudapis.eu`,
  which serves every resource wherever it lives. `core.WithRegion`, or
  `ORIGINCLOUD_REGION`, picks a region's front door instead.
- **Operations.** A create, update or delete returns a `core.Operation`; its
  `Wait` returns the resource once the change is complete, or the error the
  change failed with.
- **Retries.** A call answered as retryable is sent again with a growing delay,
  when sending it again is safe: a read, or a mutation with a request id. The
  client fills in an empty request id, so a retried create never creates twice.
- **Read-your-writes.** The client keeps the consistency token each write
  answers with and sends it with later calls, so a read right after a write, or
  after waiting on an operation, always sees it.
- **Pages.** Each list method has an `All...` counterpart that iterates over
  every page.
- **Errors.** A refused call returns a `*core.Error`; branch on its `Reason`,
  the platform's error code.

## Versions

A service package's version is its API's: `pvs/v1beta1` is the preview version
of the PVS API and may change in incompatible ways, like the API itself.

## Contributing

Issues and pull requests are welcome. The service packages are generated from
the API definitions, so a change to what a service offers starts as an issue;
see [CONTRIBUTING.md](CONTRIBUTING.md) for how the SDK is built and checked.

## License

Apache License 2.0; see [LICENSE](LICENSE).
