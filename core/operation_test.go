package core

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
)

const operationName = "projects/p/zones/z/operations/op-1"

func writeOperation(t *testing.T, w http.ResponseWriter, op *longrunningpb.Operation) {
	t.Helper()
	data, err := protojson.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(data)
}

func TestWaitWaitsUntilTheOperationIsDone(t *testing.T) {
	var waits atomic.Int32
	result, _ := structpb.NewStruct(map[string]any{"name": "projects/p/zones/z/volumes/vol-1"})
	response, _ := anypb.New(result)
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1beta1/"+operationName+":wait" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if strings.ReplaceAll(string(body), " ", "") != `{"timeout":"60s"}` {
			t.Errorf("body = %s", body)
		}
		if waits.Add(1) < 3 {
			writeOperation(t, w, &longrunningpb.Operation{Name: operationName})
			return
		}
		w.Header().Set(ConsistencyTokenHeader, "v1.done")
		writeOperation(t, w, &longrunningpb.Operation{
			Name: operationName, Done: true, Result: &longrunningpb.Operation_Response{Response: response},
		})
	})
	op := NewOperation[*structpb.Struct](client.Service("pvs", true), &longrunningpb.Operation{Name: operationName})

	got, err := op.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.GetFields()["name"].GetStringValue() != "projects/p/zones/z/volumes/vol-1" || waits.Load() != 3 {
		t.Fatalf("Wait() = %v after %d waits", got, waits.Load())
	}
	if !op.Done() || client.ConsistencyToken() != "v1.done" {
		t.Fatalf("done %v, token %q", op.Done(), client.ConsistencyToken())
	}
}

func TestAFailedOperationReturnsItsError(t *testing.T) {
	info, _ := anypb.New(&errdetails.ErrorInfo{Reason: "INTERNAL_SERVER_ERROR", Domain: "origincloudapis.eu"})
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeOperation(t, w, &longrunningpb.Operation{
			Name: operationName, Done: true,
			Result: &longrunningpb.Operation_Error{Error: &status.Status{
				Code: 13, Message: "the storage couldn't be created", Details: []*anypb.Any{info},
			}},
		})
	})
	op := NewOperation[*structpb.Struct](client.Service("pvs", true), &longrunningpb.Operation{Name: operationName})

	_, err := op.Wait(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *Error", err)
	}
	if apiErr.Status != "INTERNAL" || apiErr.Reason != "INTERNAL_SERVER_ERROR" || apiErr.Message != "the storage couldn't be created" {
		t.Fatalf("error = %+v", apiErr)
	}
}

func TestAnOperationAnsweredDoneIsNotWaitedFor(t *testing.T) {
	response, _ := anypb.New(&structpb.Struct{})
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call %s %s", r.Method, r.URL)
	})
	op := NewOperation[*structpb.Struct](client.Service("pvs", true), &longrunningpb.Operation{
		Name: operationName, Done: true, Result: &longrunningpb.Operation_Response{Response: response},
	})
	if _, err := op.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestResultOfAnOperationThatIsntDone(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {})
	op := NewOperation[*structpb.Struct](client.Service("pvs", true), &longrunningpb.Operation{Name: operationName})
	if _, err := op.Result(); err == nil {
		t.Fatal("Result() of a running operation succeeded")
	}
}
