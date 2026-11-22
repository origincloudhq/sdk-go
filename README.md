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
| `go.origincloud.eu/sdk/sts/v1beta1` | The security token service: a CI job's or another identity provider's OIDC token exchanged for a `WORKLOAD` service account's access token |
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
2. `ORIGINCLOUD_CREDENTIALS_FILE`: the path of a service account key's
   credentials file (see [A service account key](#a-service-account-key)).
   This is for automation that can present no OIDC token; CI should use the
   next step.
3. A CI job's OIDC token, exchanged at the security token service for a
   `WORKLOAD` service account's access tokens as they expire, with no secret in
   the CI system: the token in the file `ORIGINCLOUD_OIDC_TOKEN_FILE` names (any
   CI provider, a Kubernetes projected token), read again at every exchange; or
   else the token `ORIGINCLOUD_OIDC_TOKEN` holds (a GitLab CI job's `id_tokens`
   entry of that name), which can't be renewed and so serves until it expires;
   or else an HCP Terraform run's workload identity token,
   `TFC_WORKLOAD_IDENTITY_TOKEN` (for a workspace whose
   `TFC_WORKLOAD_IDENTITY_AUDIENCE` is `https://sts.origincloudapis.eu`), or
   `TFC_WORKLOAD_IDENTITY_TOKEN_<TAG>` when
   `ORIGINCLOUD_TFC_WORKLOAD_IDENTITY_TAG` names the tag of a tagged audience;
   or else, in a GitHub Actions job with `permissions: id-token: write`, the
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

### A service account key

Creating a key for an `EXTERNAL` service account returns its private key
once, in a credentials file: a JSON document naming the key (`name`), the
account (`service_account`), the private key and the security token service
(`sts_endpoint`). Save it as it is, readable only by the program that uses
it, and point `ORIGINCLOUD_CREDENTIALS_FILE` at it:

```sh
export ORIGINCLOUD_CREDENTIALS_FILE=/etc/myapp/origincloud-key.json
```

Or hand it over in code, for instance from a secret manager:

```go
credentials, err := core.CredentialsFromJSON(secret) // or core.CredentialsFromFile(path)
if err != nil {
	log.Fatal(err)
}
client, err := core.NewClient(ctx, core.WithCredentials(credentials))
```

The SDK signs an `ES256` assertion with the key (RFC 7523: `kid` the key,
`iss` and `sub` the account, `aud` the file's `sts_endpoint`, valid for five
minutes, a fresh `jti` each time), exchanges it at the security token
service's `/v1/token` for the account's access token, and exchanges a new
one about a minute before that token expires; concurrent calls share one
exchange. The credentials never print the private key.

A key never expires. To replace one, create the new key, move the program to
its file, then delete the old key; deleting a key stops its access tokens at
their next call.

## Behaviour

- **Endpoints.** Every service answers at `{service}.origincloudapis.eu`,
  which serves every resource wherever it lives. `core.WithRegion`, or
  `ORIGINCLOUD_REGION`, picks a region's front door instead,
  `{service}.{region}.origincloudapis.eu`; `location`, `resourcemanager` and
  `iam` are global and have none, so they always answer at their own host.
- **Names.** A resource's `Name` is its relative name; its full resource
  name, `//{service}.origincloudapis.eu/{name}`, is what references carry.
  `core.ParseFullName` parses one (refusing a URN, a relative name, a
  regional or zonal host, a version), `core.FullNameFromRelative` builds
  one, and a `core.FullName` gives its `Service`, `Host`, `Relative` name
  and `URL`; principals and roles are iam names (`Principal`, `Role`). A
  request's `Name` or `Parent` takes either form. A full name is checked
  against the type the field refers to: the client's own service for its
  own resources, the referenced type's service otherwise (a billing
  account's `Parent` takes `//resourcemanager.origincloudapis.eu/organizations/o`).
  It is sent as its relative name, in the path and the body alike, and a
  full name on any other host is refused before anything is sent.
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
- **Files.** A method that answers with a file rather than JSON, such as a
  PDF, returns a `google.api.HttpBody`: the file's bytes in `Data` and their
  media type in `ContentType`, as the API sent them.
- **Errors.** A refused call returns a `*core.Error`; branch on its `Reason`,
  the platform's error code. Its `Metadata` carries what the API adds to the
  reason, such as `serviceAccountKey`, the key a repeated create refused with
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
