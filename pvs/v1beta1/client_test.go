package pvsv1beta1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/fieldmaskpb"

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
