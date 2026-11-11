// Package core is what every service package of the Origin Cloud SDK
// shares: credentials, endpoints, retries, errors, long-running operations
// and read-your-writes.
//
// A program creates one [Client] and hands it to the clients of the
// services it uses:
//
//	client, err := core.NewClient(ctx)
//	if err != nil {
//		return err
//	}
//	volumes := pvsv1beta1.NewVolumeClient(client)
//
// # Credentials
//
// Unless [WithCredentials] names them, [NewClient] takes the first of:
//
//  1. ORIGINCLOUD_ACCESS_TOKEN: an access token, sent as it is.
//  2. ORIGINCLOUD_CLIENT_ID and ORIGINCLOUD_CLIENT_SECRET: a service
//     account's credential, exchanged for access tokens as they expire.
//  3. A CI job's OIDC token, exchanged at the security token service for a
//     workload service account's access tokens as they expire: the token in
//     the file ORIGINCLOUD_OIDC_TOKEN_FILE names, read again at every
//     exchange; or else the token ORIGINCLOUD_OIDC_TOKEN holds, such as the
//     one GitLab CI sets for a job's id_tokens, which can't be renewed and
//     so serves until it expires; or else, in a GitHub Actions job with
//     `permissions: id-token: write`, the job's.
//     ORIGINCLOUD_OIDC_DISABLED=true leaves this step out;
//     ORIGINCLOUD_STS_ENDPOINT asks another service than [STSEndpoint], and
//     ORIGINCLOUD_OIDC_AUDIENCE asks GitHub for another audience than
//     [OIDCAudience]. See [FederatedCredentials].
//  4. The session the origin CLI stored at its last login (origin id login).
//  5. The instance's metadata service, on an instance that runs as a
//     service account: the account's access tokens, fetched as they
//     expire. ORIGINCLOUD_METADATA_DISABLED=true leaves this step out;
//     ORIGINCLOUD_METADATA_ENDPOINT asks another address than
//     [MetadataEndpoint].
//
// # Endpoints
//
// Every service answers at {service}.origincloudapis.eu, which serves every
// resource wherever it lives. [WithRegion], or ORIGINCLOUD_REGION, picks a
// region's front door instead, {service}.{region}.origincloudapis.eu, for
// the services that have regional ones.
//
// # Retries
//
// A call the API answers as retryable is sent again, after the delay the
// answer asks for or a growing one, as long as sending it again is safe: a
// read, or a mutation with a request id. The client fills in a mutation's
// request id when it is empty, so that a retry can't apply it twice.
//
// # Read-your-writes
//
// The API answers reads from replicas that can be a moment behind a write
// that just succeeded. The client keeps the consistency token each mutation
// and each finished operation answers with, and sends it with every later
// call, so that a read through the same client always shows the client's
// own writes.
//
// # Errors
//
// A call the API refuses returns an [*Error]: branch on its Reason. Its
// Metadata carries what the API adds to the Reason, such as the name of the
// resource a CONFLICT is about:
//
//	var apiErr *core.Error
//	if errors.As(err, &apiErr) && apiErr.Reason == "CONFLICT" {
//		log.Printf("refused: %s already exists", apiErr.Metadata["serviceAccount"])
//	}
package core
