package pvsv1beta1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"go.origincloud.eu/sdk/core"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
)

// A request as the server received it.
type received struct {
	method, path, body string
	query              map[string][]string
}

// recordingClient returns a client whose calls are recorded and answered
// with an empty JSON object.
func recordingClient(t *testing.T) (*pvsv1beta1.VolumeClient, *[]received) {
	t.Helper()
	var calls []received
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, received{r.Method, r.URL.EscapedPath(), string(body), r.URL.Query()})
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	return newClient(t, server.URL), &calls
}

var uuid = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// sameJSON compares JSON by value: protojson varies its whitespace on
// purpose.
func sameJSON(got, want string) bool {
	var g, w any
	return json.Unmarshal([]byte(got), &g) == nil && json.Unmarshal([]byte(want), &w) == nil && reflect.DeepEqual(g, w)
}

func TestEachBindingIsSentAsItsHTTPRuleSays(t *testing.T) {
	volumes, calls := recordingClient(t)
	ctx := context.Background()
	name := zone + "/volumes/vol-1"

	if _, err := volumes.ListVolumes(ctx, &pvsv1beta1.ListVolumesRequest{Parent: zone, PageSize: 5, PageToken: "next"}); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.UpdateVolume(ctx, &pvsv1beta1.UpdateVolumeRequest{
		Volume:     &pvsv1beta1.Volume{Name: name, SizeGib: 2},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"size_gib", "tags"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.ImportVolume(ctx, &pvsv1beta1.ImportVolumeRequest{
		Parent: zone, Volume: &pvsv1beta1.Volume{SizeGib: 3}, SourceUrl: "https://example.com/disk.img", RequestId: "given",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.DeleteVolume(ctx, &pvsv1beta1.DeleteVolumeRequest{Name: name}); err != nil {
		t.Fatal(err)
	}

	list, update, imported, deleted := (*calls)[0], (*calls)[1], (*calls)[2], (*calls)[3]
	if list.method != "GET" || list.path != "/v1beta1/"+zone+"/volumes" || list.body != "" ||
		list.query["pageSize"][0] != "5" || list.query["pageToken"][0] != "next" || len(list.query) != 2 {
		t.Errorf("list: %+v", list)
	}
	// body: "volume": the volume is the body, the rest goes in the query.
	if update.method != "PATCH" || update.path != "/v1beta1/"+name ||
		!sameJSON(update.body, `{"name":"`+name+`","sizeGib":2}`) ||
		update.query["updateMask"][0] != "sizeGib,tags" || !uuid.MatchString(update.query["requestId"][0]) {
		t.Errorf("update: %+v", update)
	}
	// body: "*": everything is the body, nothing the query.
	if imported.method != "POST" || imported.path != "/v1beta1/"+zone+"/volumes:import" || len(imported.query) != 0 ||
		!sameJSON(imported.body, `{"parent":"`+zone+`","volume":{"sizeGib":3},"sourceUrl":"https://example.com/disk.img","requestId":"given"}`) {
		t.Errorf("import: %+v", imported)
	}
	if deleted.method != "DELETE" || deleted.path != "/v1beta1/"+name || deleted.body != "" || !uuid.MatchString(deleted.query["requestId"][0]) {
		t.Errorf("delete: %+v", deleted)
	}
}

func TestAnEmptyRequestIDIsFilledInWithoutChangingTheCallersRequest(t *testing.T) {
	volumes, calls := recordingClient(t)
	req := &pvsv1beta1.CreateVolumeRequest{Parent: zone, Volume: &pvsv1beta1.Volume{SizeGib: 1}}

	if _, err := volumes.CreateVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if req.GetRequestId() != "" {
		t.Errorf("the caller's request was changed: %q", req.GetRequestId())
	}
	if !uuid.MatchString((*calls)[0].query["requestId"][0]) {
		t.Errorf("request id %q", (*calls)[0].query["requestId"])
	}
}

func TestANameIsEscapedOnce(t *testing.T) {
	volumes, calls := recordingClient(t)

	if _, err := volumes.GetVolume(context.Background(), &pvsv1beta1.GetVolumeRequest{Name: "projects/a b%/zones/z/volumes/v"}); err != nil {
		t.Fatal(err)
	}
	if got := (*calls)[0].path; got != "/v1beta1/projects/a%20b%25/zones/z/volumes/v" {
		t.Fatalf("path = %s", got)
	}
}

func TestANameOfTheWrongFormIsRefusedBeforeSending(t *testing.T) {
	volumes, calls := recordingClient(t)

	_, err := volumes.GetVolume(context.Background(), &pvsv1beta1.GetVolumeRequest{Name: "vol-1"})
	if err == nil || !strings.Contains(err.Error(), "name must have the form projects/*/zones/*/volumes/*") {
		t.Fatalf("err = %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("sent %d calls", len(*calls))
	}
}

// A full resource name of the client's own service is sent as its
// relative name: the same request as the relative name makes.
func TestAFullNameOfTheServiceIsSentAsItsRelativeName(t *testing.T) {
	volumes, calls := recordingClient(t)
	ctx := context.Background()
	relative := zone + "/volumes/vol-1"

	for _, prefix := range []string{"", "//pvs.origincloudapis.eu/"} {
		if _, err := volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: prefix + relative}); err != nil {
			t.Fatal(err)
		}
		if _, err := volumes.ListVolumes(ctx, &pvsv1beta1.ListVolumesRequest{Parent: prefix + zone}); err != nil {
			t.Fatal(err)
		}
		if _, err := volumes.DeleteVolume(ctx, &pvsv1beta1.DeleteVolumeRequest{Name: prefix + relative, RequestId: "given"}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"GET /v1beta1/" + relative + " map[]",
		"GET /v1beta1/" + zone + "/volumes map[]",
		"DELETE /v1beta1/" + relative + " map[requestId:[given]]",
	}
	want = append(want, want...)
	var got []string
	for _, c := range *calls {
		got = append(got, fmt.Sprintf("%s %s %v", c.method, c.path, c.query))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sent\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A full name in a path field that the body also carries, as an update's
// resource does, is sent as the relative name in both, and the caller's
// request is left as it was.
func TestAFullNameInTheBodysResourceIsSentAsItsRelativeName(t *testing.T) {
	volumes, calls := recordingClient(t)
	ctx := context.Background()
	relative := zone + "/volumes/vol-1"

	for _, prefix := range []string{"", "//pvs.origincloudapis.eu/"} {
		update := &pvsv1beta1.UpdateVolumeRequest{Volume: &pvsv1beta1.Volume{Name: prefix + relative, SizeGib: 2}, RequestId: "given"}
		if _, err := volumes.UpdateVolume(ctx, update); err != nil {
			t.Fatal(err)
		}
		if update.GetVolume().GetName() != prefix+relative {
			t.Errorf("the caller's request was changed: %q", update.GetVolume().GetName())
		}
		if _, err := volumes.ImportVolume(ctx, &pvsv1beta1.ImportVolumeRequest{Parent: prefix + zone, RequestId: "given"}); err != nil {
			t.Fatal(err)
		}
	}
	c := *calls
	if len(c) != 4 {
		t.Fatalf("sent %d calls", len(c))
	}
	for i := range 2 {
		a, b := c[i], c[i+2]
		if a.method != b.method || a.path != b.path || !reflect.DeepEqual(a.query, b.query) || !sameJSON(a.body, b.body) {
			t.Errorf("relative %+v, full %+v", a, b)
		}
	}
	if !sameJSON(c[2].body, `{"name":"`+relative+`","sizeGib":2}`) {
		t.Errorf("update body %s", c[2].body)
	}
}

// A full name of another service, or one that isn't a valid full name, is
// refused before anything is sent, naming the field and the host the
// client calls.
func TestANameOfAnotherServiceIsRefusedBeforeSending(t *testing.T) {
	volumes, calls := recordingClient(t)
	ctx := context.Background()

	for name, says := range map[string]string{
		"//compute.origincloudapis.eu/projects/p/zones/z/volumes/vol-1":             "name must be a name on pvs.origincloudapis.eu, got",
		"//pvs.eu-central1-az1.origincloudapis.eu/projects/p/zones/z/volumes/vol-1": "the service's global host, pvs.origincloudapis.eu",
		"//pvs.origincloudapis.eu/v1beta1/projects/p/zones/z/volumes/vol-1":         `carries the API version "v1beta1"`,
		"urn:origincloud:storage:o:p:volume:vol-1":                                  "is a URN",
		"//pvs.origincloudapis.eu/projects/p/zones/z/volumes/vol-1/":                "ends with a '/'",
		// A full name of the service, but not of a volume.
		"//pvs.origincloudapis.eu/projects/p/zones/z/snapshots/s-1": "name must have the form projects/*/zones/*/volumes/*",
	} {
		_, err := volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: name})
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := volumes.ListVolumes(ctx, &pvsv1beta1.ListVolumesRequest{Parent: "//resourcemanager.origincloudapis.eu/projects/p"})
	if !errors.Is(err, core.ErrOtherService) || !strings.Contains(err.Error(), "parent must be a name on pvs.origincloudapis.eu") {
		t.Errorf("parent: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("sent %d calls", len(*calls))
	}
}

func TestAllVolumesFollowsThePages(t *testing.T) {
	pages := map[string]string{
		"":   `{"volumes":[{"name":"` + zone + `/volumes/vol-1"},{"name":"` + zone + `/volumes/vol-2"}],"nextPageToken":"p2"}`,
		"p2": `{"volumes":[{"name":"` + zone + `/volumes/vol-3"}]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, pages[r.URL.Query().Get("pageToken")])
	}))
	defer server.Close()
	volumes := newClient(t, server.URL)

	var names []string
	for volume, err := range volumes.AllVolumes(context.Background(), &pvsv1beta1.ListVolumesRequest{Parent: zone}) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, strings.TrimPrefix(volume.GetName(), zone+"/volumes/"))
	}
	if strings.Join(names, ",") != "vol-1,vol-2,vol-3" {
		t.Fatalf("listed %v", names)
	}
}
