package core

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	volumeName     = "//pvs.origincloudapis.eu/projects/shop-123456/zones/eu-central1-az1/volumes/vol-RAiXDqjUHLh"
	volumeRelative = "projects/shop-123456/zones/eu-central1-az1/volumes/vol-RAiXDqjUHLh"
)

func TestTheServicesAreTheAPIsWithResources(t *testing.T) {
	want := []string{"billing", "compute", "dns", "iam", "kms", "location", "network", "pvs", "resourcemanager", "support"}
	if got := ResourceServices(); !slices.Equal(got, want) {
		t.Errorf("ResourceServices() = %q, want %q", got, want)
	}
	ResourceServices()[0] = "changed"
	if ResourceServices()[0] != "billing" {
		t.Error("ResourceServices hands out its own list")
	}
}

func TestAFullNameParsesIntoItsServiceAndRelativeName(t *testing.T) {
	name, err := ParseFullName(volumeName)
	if err != nil {
		t.Fatal(err)
	}
	if name.Service() != "pvs" || name.Host() != "pvs.origincloudapis.eu" || name.Relative() != volumeRelative || name.String() != volumeName || name.IsZero() {
		t.Errorf("%q: service %q, host %q, relative %q", name, name.Service(), name.Host(), name.Relative())
	}
	var zero FullName
	if !zero.IsZero() || zero.Service() != "" || zero.Host() != "" || zero.Relative() != "" || zero.String() != "" {
		t.Errorf("the zero FullName: %+v", zero)
	}
}

func TestFullAndRelativeConvertByConcatenation(t *testing.T) {
	name, err := FullNameFromRelative("pvs", volumeRelative)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, _ := ParseFullName(volumeName); name != parsed || name.String() != volumeName {
		t.Errorf("FullNameFromRelative = %q, ParseFullName = %q", name, parsed)
	}
	for _, service := range ResourceServices() {
		name, err := FullNameFromRelative(service, "projects/p")
		if err != nil {
			t.Fatal(err)
		}
		if name.String() != "//"+service+".origincloudapis.eu/projects/p" || name.Service() != service || name.Relative() != "projects/p" {
			t.Errorf("%s: %q", service, name)
		}
		if parsed, err := ParseFullName(name.String()); err != nil || parsed != name {
			t.Errorf("%s: parsed %q, %v", service, parsed, err)
		}
	}
	a, _ := FullNameFromRelative("pvs", "projects/p")
	b, _ := FullNameFromRelative("compute", "projects/p")
	if a == b {
		t.Error("names of two services are equal")
	}
}

func TestTheURLAddsTheSchemeAndTheVersion(t *testing.T) {
	name, _ := ParseFullName(volumeName)
	for version, want := range map[string]string{
		"v1beta1": "https://pvs.origincloudapis.eu/v1beta1/" + volumeRelative,
		"v2":      "https://pvs.origincloudapis.eu/v2/" + volumeRelative,
	} {
		if got, err := name.URL(version); err != nil || got != want {
			t.Errorf("URL(%q) = %q, %v; want %q", version, got, err, want)
		}
	}
	for _, notAVersion := range []string{"", "v", "1", "V1", "v0", "v01", "v1gamma1", "v1/", "volumes"} {
		if _, err := name.URL(notAVersion); !errors.Is(err, ErrNotAVersion) {
			t.Errorf("URL(%q): %v", notAVersion, err)
		}
	}
}

func TestNamesThePlatformUsesParse(t *testing.T) {
	for _, name := range []string{
		"//iam.origincloudapis.eu/users/2f0a6c0e-7d0e-4f4b-9b8a-0c6f4e1d2a3b",
		"//iam.origincloudapis.eu/users/me",
		"//iam.origincloudapis.eu/roles/iam.editor",
		"//iam.origincloudapis.eu/roles/pvs.volume_admin",
		"//iam.origincloudapis.eu/permissions/pvs.volume.create",
		"//iam.origincloudapis.eu/organizations/o-1/serviceAccounts/9a1b",
		"//iam.origincloudapis.eu/serviceAgents/compute",
		"//iam.origincloudapis.eu/services/admin",
		"//resourcemanager.origincloudapis.eu/projects/shop-123456",
		"//billing.origincloudapis.eu/organizations/o/billingAccounts/ba/spendLimit",
		"//kms.origincloudapis.eu/projects/p/regions/eu-central1/keys/key-RAiXDqjUHLh/versions/1",
		"//location.origincloudapis.eu/zones/eu-central1-az1",
		"//billing.origincloudapis.eu/legalDocuments/terms/versions/2026-12",
		"//compute.origincloudapis.eu/zones/eu-central1-az1/images/img~1_a.b",
		// A version is refused only as the first segment.
		"//kms.origincloudapis.eu/projects/v1/regions/r/keys/k",
		"//billing.origincloudapis.eu/legalDocuments/v1",
	} {
		parsed, err := ParseFullName(name)
		if err != nil || parsed.String() != name {
			t.Errorf("%s: %q, %v", name, parsed, err)
		}
	}
}

// Each refusal, by the reason that refuses it.
func TestRefusalsNameTheirReason(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason error
		says   string
	}{
		// Not a full name at all.
		{"urn:origincloud:storage:o-1:p-1:volume:vol-1", ErrURN, "is a URN"},
		{"URN:origincloud:storage:volume:vol-1", ErrURN, "is a URN"},
		{"https://pvs.origincloudapis.eu/projects/p", ErrScheme, "is a URL"},
		{"https://pvs.origincloudapis.eu/v1beta1/projects/p", ErrScheme, "is a URL"},
		{"pvs://projects/p", ErrScheme, "is a URL"},
		{volumeRelative, ErrRelativeName, "is a relative resource name"},
		{"/projects/p", ErrRelativeName, "is a relative resource name"},
		{"pvs.origincloudapis.eu/projects/p", ErrRelativeName, "is a relative resource name"},
		{"vol-RAiXDqjUHLh", ErrBareID, "is an id"},
		// The host.
		{"//example.com/projects/p", ErrUnknownService, "is not the host of one of the platform's APIs"},
		{"//pvs.origincloudapis.com/projects/p", ErrUnknownService, "pvs.origincloudapis.com is not the host"},
		{"//PVS.origincloudapis.eu/projects/p", ErrUnknownService, ""},
		{"//pvs.origincloudapis.eu:443/projects/p", ErrUnknownService, ""},
		{"//sts.origincloudapis.eu/projects/p", ErrUnknownService, ""},
		{"//observability.origincloudapis.eu/projects/p", ErrUnknownService, ""},
		{"//graph.origincloudapis.eu/projects/p", ErrUnknownService, ""},
		{"//origincloudapis.eu/projects/p", ErrUnknownService, ""},
		{"///projects/p", ErrUnknownService, ""},
		{"//pvs.eu-central1-az1.origincloudapis.eu/projects/p", ErrNotGlobalHost, "the service's global host, pvs.origincloudapis.eu"},
		{"//network.eu-central1.origincloudapis.eu/projects/p", ErrNotGlobalHost, "the service's global host, network.origincloudapis.eu"},
		// No relative name.
		{"//pvs.origincloudapis.eu", ErrNoRelativeName, "has no relative name"},
		{"//pvs.origincloudapis.eu/", ErrNoRelativeName, "has no relative name"},
		{"//", ErrUnknownService, ""},
		// A version.
		{"//pvs.origincloudapis.eu/v1beta1/projects/p", ErrVersion, `the API version "v1beta1"`},
		{"//pvs.origincloudapis.eu/v1/projects/p", ErrVersion, `the API version "v1"`},
		// The segments.
		{"//pvs.origincloudapis.eu/projects/p/", ErrTrailingSlash, "ends with a '/'"},
		{"//pvs.origincloudapis.eu/projects//p", ErrEmptySegment, "an empty segment"},
		{"//pvs.origincloudapis.eu//projects/p", ErrEmptySegment, "an empty segment"},
		{"//pvs.origincloudapis.eu/projects/./volumes", ErrDotSegment, `the segment "."`},
		{"//pvs.origincloudapis.eu/projects/p/../q", ErrDotSegment, `the segment ".."`},
		{"//pvs.origincloudapis.eu/projects/p q", ErrInvalidSegment, `the segment "p q"`},
		{"//pvs.origincloudapis.eu/projects/p%2Fq", ErrInvalidSegment, ""},
		{"//pvs.origincloudapis.eu/projects/p?view=full", ErrInvalidSegment, ""},
		{"//pvs.origincloudapis.eu/projects/p#x", ErrInvalidSegment, ""},
		{"//pvs.origincloudapis.eu/projects/p:q", ErrInvalidSegment, ""},
		{"//pvs.origincloudapis.eu/projects/pä", ErrInvalidSegment, ""},
		{"//pvs.origincloudapis.eu/projects/p\n", ErrInvalidSegment, ""},
	} {
		_, err := ParseFullName(c.name)
		if !errors.Is(err, c.reason) {
			t.Errorf("%q: refused as %v, want %v", c.name, err, c.reason)
			continue
		}
		var e *FullNameError
		if !errors.As(err, &e) || e.Value != c.name {
			t.Errorf("%q: %#v", c.name, err)
		}
		if !strings.Contains(err.Error(), strconv.Quote(c.name)) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%q: %v", c.name, err)
		}
	}
	if _, err := ParseFullName(""); !errors.Is(err, ErrEmptyName) {
		t.Errorf("empty: %v", err)
	}
}

func TestFromRelativeRefusesWhatParseRefusesInARelativeName(t *testing.T) {
	for relative, reason := range map[string]error{
		"":                   ErrNoRelativeName,
		"/projects/p":        ErrEmptySegment,
		"projects/p/":        ErrTrailingSlash,
		"projects//p":        ErrEmptySegment,
		"projects/..":        ErrDotSegment,
		"v1beta1/projects/p": ErrVersion,
		"projects/p q":       ErrInvalidSegment,
		volumeName:           ErrNotRelative,
	} {
		if _, err := FullNameFromRelative("pvs", relative); !errors.Is(err, reason) {
			t.Errorf("%q: %v, want %v", relative, err, reason)
		}
	}
	for _, service := range []string{"sts", "observability", "graph", "", "PVS", "pvs.origincloudapis.eu"} {
		if _, err := FullNameFromRelative(service, "projects/p"); !errors.Is(err, ErrUnknownService) {
			t.Errorf("service %q: %v", service, err)
		}
	}
	if _, err := ParseFullName(volumeRelative); !errors.Is(err, ErrRelativeName) {
		t.Errorf("a relative name parsed: %v", err)
	}
}

func TestJSONGivesAndTakesTheFullForm(t *testing.T) {
	name, _ := ParseFullName(volumeName)
	encoded, err := json.Marshal(struct{ Volume FullName }{name})
	if err != nil || string(encoded) != `{"Volume":"`+volumeName+`"}` {
		t.Fatalf("%s, %v", encoded, err)
	}
	var decoded struct{ Volume FullName }
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Volume != name {
		t.Fatalf("%q, %v", decoded.Volume, err)
	}
	for _, refused := range []string{
		volumeRelative,
		"vol-RAiXDqjUHLh",
		"urn:origincloud:storage:o:p:volume:v",
		"//pvs.eu-central1-az1.origincloudapis.eu/projects/p",
		"//pvs.origincloudapis.eu/v1beta1/projects/p",
		"//pvs.origincloudapis.eu/projects/p/",
	} {
		var into FullName
		_, want := ParseFullName(refused)
		if err := json.Unmarshal([]byte(strconv.Quote(refused)), &into); err == nil || !strings.Contains(err.Error(), want.Error()) {
			t.Errorf("%q: %v", refused, err)
		}
	}
}

func TestEachPrincipalKindHasItsOwnForm(t *testing.T) {
	for full, want := range map[string]Principal{
		"//iam.origincloudapis.eu/users/u-1":                              {Kind: PrincipalUser, ID: "u-1"},
		"//iam.origincloudapis.eu/organizations/o-1/serviceAccounts/sa-1": {Kind: PrincipalServiceAccount, Organization: "o-1", ID: "sa-1"},
		"//iam.origincloudapis.eu/serviceAgents/compute":                  {Kind: PrincipalServiceAgent, ID: "compute"},
		"//iam.origincloudapis.eu/services/admin":                         {Kind: PrincipalService, ID: "admin"},
	} {
		name, _ := ParseFullName(full)
		if got, ok := name.Principal(); !ok || got != want {
			t.Errorf("%s: %+v, %v", full, got, ok)
		}
		if built, err := want.FullName(); err != nil || built != name {
			t.Errorf("%+v: %q, %v", want, built, err)
		}
		if _, ok := name.Role(); ok {
			t.Errorf("%s is a role", full)
		}
	}
	for _, full := range []string{
		// The organisation principal is not decided.
		"//iam.origincloudapis.eu/organizations/o-1",
		"//resourcemanager.origincloudapis.eu/organizations/o-1",
		// A principal's spelling on another service.
		"//resourcemanager.origincloudapis.eu/users/u-1",
		// A child of a principal.
		"//iam.origincloudapis.eu/organizations/o/serviceAccounts/sa/federatedCredentials/f",
		"//iam.origincloudapis.eu/users/u-1/sessions/s",
		"//iam.origincloudapis.eu/roles/iam.editor",
	} {
		name, _ := ParseFullName(full)
		if p, ok := name.Principal(); ok {
			t.Errorf("%s: %+v", full, p)
		}
	}
	for _, p := range []Principal{
		{Kind: PrincipalUser, ID: "u/1"},
		{Kind: PrincipalServiceAccount, Organization: "o", ID: "sa/keys/k"},
		{Kind: PrincipalServiceAccount, Organization: "o/x", ID: "sa"},
		{Kind: PrincipalServiceAgent, ID: "a/b"},
		{Kind: PrincipalService, ID: ""},
		{ID: "u-1"},
	} {
		if name, err := p.FullName(); err == nil {
			t.Errorf("%+v: %q", p, name)
		}
	}
}

func TestARoleIsNamedWithDotsAndNeverSlashes(t *testing.T) {
	role, _ := ParseFullName("//iam.origincloudapis.eu/roles/iam.editor")
	if id, ok := role.Role(); !ok || id != "iam.editor" {
		t.Errorf("Role() = %q, %v", id, ok)
	}
	if built, err := RoleName("iam.editor"); err != nil || built != role {
		t.Errorf("RoleName = %q, %v", built, err)
	}
	slashed, _ := ParseFullName("//iam.origincloudapis.eu/roles/iam/editor")
	if id, ok := slashed.Role(); ok {
		t.Errorf("roles/iam/editor is the role %q", id)
	}
	elsewhere, _ := ParseFullName("//resourcemanager.origincloudapis.eu/roles/iam.editor")
	if _, ok := elsewhere.Role(); ok {
		t.Error("a role on resourcemanager")
	}
	for _, bad := range []string{"iam/editor", ""} {
		if _, err := RoleName(bad); err == nil {
			t.Errorf("RoleName(%q) passed", bad)
		}
	}
}

// A path field takes a relative name, or a full name of the client's own
// service, which is sent as its relative name.
func TestAPathNameIsTheRelativeNameOfTheClientsOwnService(t *testing.T) {
	for value, sent := range map[string]string{
		volumeName:             volumeRelative,
		volumeRelative:         volumeRelative,
		"projects/p 1/zones/z": "projects/p 1/zones/z",
		"":                     "",
	} {
		if got, err := pathName("pvs", "name", value); err != nil || got != sent {
			t.Errorf("%q: %q, %v", value, got, err)
		}
	}
}

func TestAPathNameOfAnotherHostOrNoFullNameIsRefused(t *testing.T) {
	for _, c := range []struct {
		value  string
		reason error
		says   string
	}{
		{"//compute.origincloudapis.eu/projects/p/zones/z/instances/i", ErrOtherService, "name must be a name on pvs.origincloudapis.eu, the host this client calls"},
		{"//resourcemanager.origincloudapis.eu/projects/p", ErrOtherService, "a name on resourcemanager.origincloudapis.eu"},
		{"//pvs.eu-central1-az1.origincloudapis.eu/projects/p/zones/z/volumes/v", ErrNotGlobalHost, "the service's global host, pvs.origincloudapis.eu"},
		{"//pvs.eu-central1.origincloudapis.eu/projects/p/zones/z/volumes/v", ErrNotGlobalHost, "the service's global host"},
		{"//pvs.origincloudapis.eu/v1beta1/projects/p/zones/z/volumes/v", ErrVersion, "carries the API version"},
		{"urn:origincloud:storage:o:p:volume:v", ErrURN, "is a URN"},
		{"https://pvs.origincloudapis.eu/v1beta1/projects/p/zones/z/volumes/v", ErrScheme, "is a URL"},
		{"//pvs.origincloudapis.eu/projects/p/zones/z/volumes/", ErrTrailingSlash, "ends with a '/'"},
		{"//pvs.origincloudapis.eu/projects/p q/zones/z/volumes/v", ErrInvalidSegment, "has the segment"},
		{"//example.com/projects/p", ErrUnknownService, "is not the host of one of the platform's APIs"},
	} {
		_, err := pathName("pvs", "name", c.value)
		if !errors.Is(err, c.reason) || !strings.HasPrefix(err.Error(), "origincloud: name") || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%q: %v", c.value, err)
		}
	}
	// A service with no resources takes no full name at all.
	if _, err := pathName("observability", "parent", "//observability.origincloudapis.eu/projects/p"); err == nil {
		t.Error("observability took a full name")
	}
}
