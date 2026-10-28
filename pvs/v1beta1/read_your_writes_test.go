package pvsv1beta1_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"go.origincloud.eu/sdk/core"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
)

// A front door whose read model trails each write by lag, as the API's
// does: a read presenting a token waits up to bound for the read model to
// reach it, then answers UNAVAILABLE; a read without one answers what the
// read model has, NOT_FOUND for a volume it hasn't seen yet.
type laggingFrontDoor struct {
	t     *testing.T
	lag   time.Duration
	bound time.Duration

	mu      sync.Mutex
	visible []time.Time // when each write, by position from 1, reaches the read model
}

const zone = "projects/p/zones/z"

func (f *laggingFrontDoor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	presented := position(r.Header.Get(core.ConsistencyTokenHeader))
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1beta1/"+zone+"/volumes":
		var volume pvsv1beta1.Volume
		body, _ := io.ReadAll(r.Body)
		if err := protojson.Unmarshal(body, &volume); err != nil || r.URL.Query().Get("requestId") == "" {
			f.t.Errorf("create: %v, request id %q", err, r.URL.Query().Get("requestId"))
		}
		f.mu.Lock()
		f.visible = append(f.visible, time.Now().Add(f.lag))
		written := len(f.visible)
		f.mu.Unlock()
		w.Header().Set(core.ConsistencyTokenHeader, token(max(presented, written)))
		f.write(w, &longrunningpb.Operation{Name: fmt.Sprintf("%s/operations/op-%d", zone, written)})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":wait"):
		written, _ := strconv.Atoi(strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "op-")+3:], ":wait"))
		response, _ := anypb.New(&pvsv1beta1.Volume{Name: volumeName(written), State: pvsv1beta1.Volume_AVAILABLE, SizeGib: 1})
		w.Header().Set(core.ConsistencyTokenHeader, token(max(presented, written)))
		f.write(w, &longrunningpb.Operation{
			Name: fmt.Sprintf("%s/operations/op-%d", zone, written), Done: true,
			Result: &longrunningpb.Operation_Response{Response: response},
		})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1beta1/"+zone+"/volumes/vol-"):
		wanted, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/v1beta1/"+zone+"/volumes/vol-"))
		deadline := time.Now().Add(f.bound)
		for f.readModel() < presented {
			if time.Now().After(deadline) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":{"code":503,"status":"UNAVAILABLE","message":"the read model hasn't reached the token yet","details":[
					{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_UNAVAILABLE","domain":"origincloudapis.eu"},
					{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.020s"}]}}`)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		if f.readModel() < wanted {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"status":"NOT_FOUND","message":"volume not found","details":[
				{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"NOT_FOUND","domain":"origincloudapis.eu"}]}}`)
			return
		}
		f.write(w, &pvsv1beta1.Volume{Name: volumeName(wanted), State: pvsv1beta1.Volume_AVAILABLE, SizeGib: 1})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

// readModel is the position the read model has reached.
func (f *laggingFrontDoor) readModel() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	reached := 0
	for reached < len(f.visible) && !time.Now().Before(f.visible[reached]) {
		reached++
	}
	return reached
}

func (f *laggingFrontDoor) write(w http.ResponseWriter, m proto.Message) {
	data, err := protojson.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	_, _ = w.Write(data)
}

func token(position int) string { return "v1." + strconv.Itoa(position) }

func position(token string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(token, "v1."))
	return n
}

func volumeName(position int) string { return fmt.Sprintf("%s/volumes/vol-%d", zone, position) }

func newClient(t *testing.T, endpoint string) *pvsv1beta1.VolumeClient {
	t.Helper()
	client, err := core.NewClient(context.Background(), core.WithEndpoint(endpoint), core.WithCredentials(core.AccessToken("t")))
	if err != nil {
		t.Fatal(err)
	}
	return pvsv1beta1.NewVolumeClient(client)
}

// A get right after a create's operation is done finds the volume, every
// time, although the read model is behind the create.
func TestAGetAfterACreateIsDoneFindsTheVolume(t *testing.T) {
	frontDoor := &laggingFrontDoor{t: t, lag: 150 * time.Millisecond, bound: 50 * time.Millisecond}
	server := httptest.NewServer(frontDoor)
	defer server.Close()
	volumes := newClient(t, server.URL)
	ctx := context.Background()

	for i := range 20 {
		op, err := volumes.CreateVolume(ctx, &pvsv1beta1.CreateVolumeRequest{
			Parent: zone, Volume: &pvsv1beta1.Volume{SizeGib: 1},
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		created, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}

		// The control: a client that didn't write doesn't see it yet, so
		// the read model really is behind.
		if _, err := newClient(t, server.URL).GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: created.GetName()}); core.ReasonOf(err) != "NOT_FOUND" {
			t.Fatalf("get %d without the token: %v, want NOT_FOUND from the lagging read model", i, err)
		}

		got, err := volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: created.GetName()})
		if err != nil {
			t.Fatalf("get %d after its create: %v", i, err)
		}
		if got.GetName() != created.GetName() {
			t.Fatalf("get %d = %s, want %s", i, got.GetName(), created.GetName())
		}
	}
}
