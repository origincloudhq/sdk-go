package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Domain is the domain every service's host is under.
const Domain = "origincloudapis.eu"

// ResourceServices returns the services that have resources, by their
// short code, in order: the domains of the resource types the API declares
// (pvs.origincloudapis.eu/Volume), and so the hosts a full resource name may
// start with, "{service}.origincloudapis.eu". A service without resources
// of its own, such as sts, names nothing.
func ResourceServices() []string { return slices.Clone(resourceServices) }

// A FullName is a full resource name (AIP-122): "//", the global host of
// the service that owns the resource, "/" and its relative name, such as
// "//pvs.origincloudapis.eu/projects/p/zones/eu-central1-az1/volumes/vol-1",
// with no scheme and no API version.
//
// A resource's name field is its relative name; references in request
// bodies are full names. A request's path fields (name, parent) take
// either: a client given a full name of its own service sends its relative
// name, and refuses one of another service before sending anything.
//
// A FullName is made only by [ParseFullName] or [FullNameFromRelative],
// which refuse anything but the one canonical spelling, so two names are
// equal (==) exactly when they name the same resource. The zero FullName
// names nothing. String, MarshalText and UnmarshalText use the full form,
// and only it.
type FullName struct {
	// "//{service}.{Domain}/{relative}", kept whole.
	full string
	// The length of the service's short code.
	serviceLen int
}

// The reasons a string is not a full resource name, which [FullNameError]
// unwraps to: errors.Is(err, core.ErrURN) tells a URN.
var (
	ErrEmptyName      = errors.New("an empty name")
	ErrURN            = errors.New("a URN")
	ErrScheme         = errors.New("a URL")
	ErrRelativeName   = errors.New("a relative name")
	ErrBareID         = errors.New("a bare id")
	ErrUnknownService = errors.New("not a service's host")
	ErrNotGlobalHost  = errors.New("a regional or zonal host")
	ErrVersion        = errors.New("an API version in the name")
	ErrNoRelativeName = errors.New("no relative name")
	ErrNotRelative    = errors.New("a full name where a relative one is expected")
	ErrTrailingSlash  = errors.New("a trailing slash")
	ErrEmptySegment   = errors.New("an empty segment")
	ErrDotSegment     = errors.New("a dot segment")
	ErrInvalidSegment = errors.New("an invalid segment")
	ErrNotAVersion    = errors.New("not an API version")
	// ErrOtherService is a request's path field given a full name of
	// another service than the one the client calls.
	ErrOtherService = errors.New("a name of another service")
)

// A FullNameError says why a string is not a full resource name, or not
// one that can be used where it was given. It unwraps to its Reason, one of
// the Err... values above.
type FullNameError struct {
	// Value is what was given.
	Value string
	// Reason is one of the Err... values.
	Reason error
	detail string
}

func (e *FullNameError) Error() string { return "origincloud: " + e.detail }

// Unwrap returns the reason.
func (e *FullNameError) Unwrap() error { return e.Reason }

func nameError(value string, reason error, format string, args ...any) error {
	return &FullNameError{Value: value, Reason: reason, detail: fmt.Sprintf(format, args...)}
}

// ParseFullName parses the full form, "//{host}/{relative}", and nothing
// else.
//
// Refused: a URN, a URL (any scheme), a relative name, a bare id, a host
// that isn't the global host of one of [ResourceServices] (a regional or
// zonal one included), an API version as the first segment, a trailing
// "/", and an empty, ".", ".." or otherwise invalid segment.
func ParseFullName(name string) (FullName, error) {
	rest, ok := strings.CutPrefix(name, "//")
	if !ok {
		return FullName{}, notFull(name)
	}
	host, relative, ok := strings.Cut(rest, "/")
	if !ok {
		if _, err := serviceOfHost(name, rest); err != nil {
			return FullName{}, err
		}
		return FullName{}, nameError(name, ErrNoRelativeName, "%q has no relative name after its service", name)
	}
	service, err := serviceOfHost(name, host)
	if err != nil {
		return FullName{}, err
	}
	if relative == "" {
		return FullName{}, nameError(name, ErrNoRelativeName, "%q has no relative name after its service", name)
	}
	if err := checkRelative(name, relative); err != nil {
		return FullName{}, err
	}
	return FullName{full: name, serviceLen: len(service)}, nil
}

// FullNameFromRelative returns the full name of the resource relative (its
// name) of service, a short code such as "pvs":
// "//{service}.origincloudapis.eu/" + relative.
//
// Refused: a service that isn't one of [ResourceServices], a full name
// given as the relative one, an API version as the first segment, a
// trailing "/", and an empty, ".", ".." or otherwise invalid segment.
func FullNameFromRelative(service, relative string) (FullName, error) {
	if strings.HasPrefix(relative, "//") {
		return FullName{}, nameError(relative, ErrNotRelative, "%q is a full resource name; a relative one is expected here", relative)
	}
	host := service + "." + Domain
	if !slices.Contains(resourceServices, service) {
		return FullName{}, nameError(relative, ErrUnknownService, "%q names no resource: %s is not the host of one of the platform's APIs", relative, host)
	}
	if relative == "" {
		return FullName{}, nameError(relative, ErrNoRelativeName, "%q has no relative name after its service", relative)
	}
	if err := checkRelative(relative, relative); err != nil {
		return FullName{}, err
	}
	return FullName{full: "//" + host + "/" + relative, serviceLen: len(service)}, nil
}

// Service returns the short code of the service that owns the resource,
// such as "pvs".
func (n FullName) Service() string {
	if n.full == "" {
		return ""
	}
	return n.full[2 : 2+n.serviceLen]
}

// Host returns the service's host, such as "pvs.origincloudapis.eu".
func (n FullName) Host() string {
	if n.full == "" {
		return ""
	}
	return n.full[2:n.hostEnd()]
}

// Relative returns the relative name, the resource's name: the full name
// without "//{host}/".
func (n FullName) Relative() string {
	if n.full == "" {
		return ""
	}
	return n.full[n.hostEnd()+1:]
}

// String returns the full form.
func (n FullName) String() string { return n.full }

// IsZero reports whether n is the zero FullName, which names nothing.
func (n FullName) IsZero() bool { return n.full == "" }

// URL returns the URL that calls the resource,
// "https://{host}/{version}/{relative}", for an API version such as
// "v1beta1".
func (n FullName) URL(version string) (string, error) {
	if !isVersion(version) {
		return "", nameError(version, ErrNotAVersion, "%q is not an API version, such as v1 or v1beta1", version)
	}
	return "https://" + n.Host() + "/" + version + "/" + n.Relative(), nil
}

// MarshalText returns the full form.
func (n FullName) MarshalText() ([]byte, error) { return []byte(n.full), nil }

// UnmarshalText parses the full form, refusing what [ParseFullName]
// refuses.
func (n *FullName) UnmarshalText(text []byte) error {
	parsed, err := ParseFullName(string(text))
	if err != nil {
		return err
	}
	*n = parsed
	return nil
}

func (n FullName) hostEnd() int { return 2 + n.serviceLen + 1 + len(Domain) }

// A PrincipalKind is the kind of a [Principal].
type PrincipalKind int

// The kinds of principal, each an iam full name.
const (
	// PrincipalUser is a user: //iam.origincloudapis.eu/users/{user}.
	PrincipalUser PrincipalKind = iota + 1
	// PrincipalServiceAccount is an organization's service account:
	// //iam.origincloudapis.eu/organizations/{organization}/serviceAccounts/{service_account}.
	PrincipalServiceAccount
	// PrincipalServiceAgent is a service agent, a platform service acting
	// in a customer's project: //iam.origincloudapis.eu/serviceAgents/{service_agent}.
	PrincipalServiceAgent
	// PrincipalService is an internal platform service:
	// //iam.origincloudapis.eu/services/{service}.
	PrincipalService
)

// A Principal is who acts: a member of a role binding, or the actor of an
// audit entry. An organisation acting on its own has no full name yet, and
// so no kind.
type Principal struct {
	Kind PrincipalKind
	// Organization is a service account's organization; empty for the
	// other kinds.
	Organization string
	// ID is the user's, service account's, service agent's or service's
	// id.
	ID string
}

// Principal returns the principal n is, if it is one.
func (n FullName) Principal() (Principal, bool) {
	if n.Service() != "iam" {
		return Principal{}, false
	}
	s := strings.Split(n.Relative(), "/")
	switch {
	case len(s) == 2 && s[0] == "users":
		return Principal{Kind: PrincipalUser, ID: s[1]}, true
	case len(s) == 4 && s[0] == "organizations" && s[2] == "serviceAccounts":
		return Principal{Kind: PrincipalServiceAccount, Organization: s[1], ID: s[3]}, true
	case len(s) == 2 && s[0] == "serviceAgents":
		return Principal{Kind: PrincipalServiceAgent, ID: s[1]}, true
	case len(s) == 2 && s[0] == "services":
		return Principal{Kind: PrincipalService, ID: s[1]}, true
	}
	return Principal{}, false
}

// FullName returns the principal's full name.
func (p Principal) FullName() (FullName, error) {
	var relative string
	parts := []string{p.ID}
	switch p.Kind {
	case PrincipalUser:
		relative = "users/" + p.ID
	case PrincipalServiceAccount:
		relative = "organizations/" + p.Organization + "/serviceAccounts/" + p.ID
		parts = append(parts, p.Organization)
	case PrincipalServiceAgent:
		relative = "serviceAgents/" + p.ID
	case PrincipalService:
		relative = "services/" + p.ID
	default:
		return FullName{}, fmt.Errorf("origincloud: %d is no kind of principal", p.Kind)
	}
	// A part with a "/" would make another name, of no principal.
	for _, part := range parts {
		if strings.Contains(part, "/") {
			return FullName{}, nameError(relative, ErrInvalidSegment, "%q has the segment %q, which a principal's id can't", relative, part)
		}
	}
	return FullNameFromRelative("iam", relative)
}

// Role returns the role's id if n is a role,
// //iam.origincloudapis.eu/roles/{role}, such as "iam.editor". A role's
// id writes its parts with dots; roles/iam/editor is no role.
func (n FullName) Role() (string, bool) {
	if n.Service() != "iam" {
		return "", false
	}
	role, ok := strings.CutPrefix(n.Relative(), "roles/")
	if !ok || strings.Contains(role, "/") {
		return "", false
	}
	return role, true
}

// RoleName returns the full name of the role role, such as "iam.editor":
// //iam.origincloudapis.eu/roles/iam.editor.
func RoleName(role string) (FullName, error) {
	if strings.Contains(role, "/") {
		return FullName{}, nameError(role, ErrInvalidSegment, "%q has a '/': a role's id writes its parts with dots, such as iam.editor", role)
	}
	return FullNameFromRelative("iam", "roles/"+role)
}

// serviceOfHost returns the short code of the service whose global host is
// host, or why there is none.
func serviceOfHost(name, host string) (string, error) {
	code, ok := strings.CutSuffix(host, "."+Domain)
	if ok && slices.Contains(resourceServices, code) {
		return code, nil
	}
	// "{service}.{region or zone}.origincloudapis.eu": a front door, not
	// the service's name.
	if ok {
		if service, location, found := strings.Cut(code, "."); found && location != "" && slices.Contains(resourceServices, service) {
			return "", nameError(name, ErrNotGlobalHost, "%q names %s; a full resource name has the service's global host, %s.%s", name, host, service, Domain)
		}
	}
	return "", nameError(name, ErrUnknownService, "%q names no resource: %s is not the host of one of the platform's APIs", name, host)
}

// notFull says why name, which doesn't start with "//", is not a full name.
func notFull(name string) error {
	switch {
	case name == "":
		return nameError(name, ErrEmptyName, "an empty string names no resource")
	case len(name) >= 4 && strings.EqualFold(name[:4], "urn:"):
		return nameError(name, ErrURN, "%q is a URN; a resource is named by its full resource name, //{service}.%s/{name}", name, Domain)
	case strings.Contains(name, "://"):
		return nameError(name, ErrScheme, "%q is a URL; a full resource name has no scheme, //{service}.%s/{name}", name, Domain)
	case strings.Contains(name, "/"):
		return nameError(name, ErrRelativeName, "%q is a relative resource name; a full one starts with //{service}.%s/", name, Domain)
	default:
		return nameError(name, ErrBareID, "%q is an id; a resource is named by its full resource name, //{service}.%s/{name}", name, Domain)
	}
}

// checkRelative checks a non-empty relative name; name is what an error
// reports.
func checkRelative(name, relative string) error {
	if strings.HasSuffix(relative, "/") {
		return nameError(name, ErrTrailingSlash, "%q ends with a '/'", name)
	}
	segments := strings.Split(relative, "/")
	if isVersion(segments[0]) {
		return nameError(name, ErrVersion, "%q carries the API version %q; a full resource name has none", name, segments[0])
	}
	for _, segment := range segments {
		switch {
		case segment == "":
			return nameError(name, ErrEmptySegment, "%q has an empty segment", name)
		case segment == "." || segment == "..":
			return nameError(name, ErrDotSegment, "%q has the segment %q, which a resource name can't", name, segment)
		case !unreserved(segment):
			return nameError(name, ErrInvalidSegment, "%q has the segment %q: a segment is letters, digits and '-', '.', '_' or '~'", name, segment)
		}
	}
	return nil
}

// unreserved reports whether s is made of RFC 3986's unreserved
// characters, so that a name is a URL path as it is.
func unreserved(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

// isVersion reports whether segment reads as an API version (AIP-185):
// "v{major}", optionally followed by "alpha" or "beta" and a number.
func isVersion(segment string) bool {
	rest, ok := strings.CutPrefix(segment, "v")
	if !ok {
		return false
	}
	major := 0
	for major < len(rest) && '0' <= rest[major] && rest[major] <= '9' {
		major++
	}
	if major == 0 || rest[0] == '0' {
		return false
	}
	stability := rest[major:]
	number, ok := strings.CutPrefix(stability, "alpha")
	if !ok {
		number, ok = strings.CutPrefix(stability, "beta")
	}
	if !ok {
		return stability == ""
	}
	for i := 0; i < len(number); i++ {
		if number[i] < '0' || number[i] > '9' {
			return false
		}
	}
	return true
}

// pathName returns the relative name to send in a request's path for
// value, the path field field of a call to the service own (a short
// code): value as it is when it isn't a full name, its relative name when
// it is one of own's, and an error when it is another service's, a URN, a
// URL or no valid full name.
func pathName(own, field, value string) (string, error) {
	inField := func(err error) error {
		var e *FullNameError
		if errors.As(err, &e) {
			return &FullNameError{Value: e.Value, Reason: e.Reason, detail: field + ": " + e.detail}
		}
		return err
	}
	if !strings.HasPrefix(value, "//") {
		if err := notFull(value); errors.Is(err, ErrURN) || errors.Is(err, ErrScheme) {
			return "", inField(err)
		}
		return value, nil
	}
	name, err := ParseFullName(value)
	if err != nil {
		return "", inField(err)
	}
	if name.Service() != own {
		return "", nameError(value, ErrOtherService, "%s must be a name on %s.%s, the host this client calls, got %q, a name on %s", field, own, Domain, value, name.Host())
	}
	return name.Relative(), nil
}
