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
//  3. The session the origin CLI stored at its last login (origin id login).
//
// # Endpoints
//
// Every service answers at {service}.origincloudapis.com, which serves every
// resource wherever it lives. [WithRegion], or ORIGINCLOUD_REGION, picks a
// region's front door instead, {service}.{region}.origincloudapis.com, for
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
// A call the API refuses returns an [*Error]: branch on its Reason.
package core
