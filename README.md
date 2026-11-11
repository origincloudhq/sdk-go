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
| `go.origincloud.eu/sdk/compute/v1beta1` | Instances, images, key pairs, volume attachments and the instance catalog |
| `go.origincloud.eu/sdk/network/v1beta1` | VPCs, subnets, routing, gateways, floating IPs, network interfaces, security groups, ACLs and DNS policies |
| `go.origincloud.eu/sdk/pvs/v1beta1` | Persistent volumes: volumes, snapshots, storage classes |
| `go.origincloud.eu/sdk/location/v1beta1` | Regions and zones, and the services each zone offers |
| `go.origincloud.eu/sdk/resourcemanager/v1beta1` | Organizations, projects and their quotas |
| `go.origincloud.eu/sdk/iam/v1beta1` | Users, service accounts, roles, role bindings, permissions and the audit log |
| `go.origincloud.eu/sdk/billing/v1beta1` | Billing accounts and the projects they pay for, payment methods, cost and usage, spend limits and the price list |
| `go.origincloud.eu/sdk/support/v1beta1` | Support cases and their messages |
| `go.origincloud.eu/sdk/dns/v1beta1` | Hosted zones, record sets and private zones' VPC attachments |
| `go.origincloud.eu/sdk/observability/v1beta1` | The metrics of a project's compute instances, volumes and network interfaces |
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
   automation outside CI should use.
3. A CI job's OIDC token, exchanged at the security token service for a
   `WORKLOAD` service account's access tokens as they expire, with no secret in
   the CI system: the token in the file `ORIGINCLOUD_OIDC_TOKEN_FILE` names (any
   CI provider, a Kubernetes projected token), read again at every exchange, or
   else, in a GitHub Actions job with `permissions: id-token: write`, the
   job's. A federated credential on the service account decides which jobs'
   tokens it trusts. `ORIGINCLOUD_OIDC_DISABLED=true` leaves this step out;
   `ORIGINCLOUD_STS_ENDPOINT` asks another service than
   `https://sts.origincloudapis.eu`, and `ORIGINCLOUD_OIDC_AUDIENCE` asks GitHub
   for another audience than `https://sts.origincloudapis.eu`.
4. The session the `origin` CLI stored at its last login (`origin id login`).
5. The instance's metadata service, on an Origin Cloud instance that runs as a
   service account: the account's access tokens, fetched as they expire, with
   no secret on the instance. `ORIGINCLOUD_METADATA_DISABLED=true` leaves this
   step out; `ORIGINCLOUD_METADATA_ENDPOINT` asks another address than
   `http://169.254.169.254`.

`core.WithCredentials` sets them explicitly instead.

## Behaviour

- **Endpoints.** Every service answers at `{service}.origincloudapis.eu`,
  which serves every resource wherever it lives. `core.WithRegion`, or
  `ORIGINCLOUD_REGION`, picks a region's front door instead,
  `{service}.{region}.origincloudapis.eu`; `location`, `resourcemanager` and
  `iam` are global and have none, so they always answer at their own host.
- **Operations.** A change the API carries out after answering, such as
  creating a volume or an instance, returns a `core.Operation`; its `Wait`
  returns the resource once the change is complete, or the error the change
  failed with.
- **Retries.** A call answered as retryable is sent again with a growing delay,
  when sending it again is safe: a read, or a mutation with a request id. The
  client fills in an empty request id, so a retried create never creates twice.
- **Read-your-writes.** The client keeps the consistency token each write
  answers with and sends it with later calls, so a read right after a write, or
  after waiting on an operation, always sees it.
- **Pages.** Each list method has an `All...` counterpart that iterates over
  every page.
- **Errors.** A refused call returns a `*core.Error`; branch on its `Reason`,
  the platform's error code. Its `Metadata` carries what the API adds to the
  reason, such as `serviceAccount`, the account a repeated create refused with
  `CONFLICT` is about. Only an error the API answers as retryable is retried:
  a `CONFLICT` never is.

## Versions

A service package's version is its API's: `pvs/v1beta1` is the preview version
of the PVS API and may change in incompatible ways, like the API itself.

## Contributing

Issues and pull requests are welcome. The service packages are generated from
the API definitions, so a change to what a service offers starts as an issue;
see [CONTRIBUTING.md](CONTRIBUTING.md) for how the SDK is built and checked.

## License

Apache License 2.0; see [LICENSE](LICENSE).
