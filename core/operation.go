package core

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// The longest the API waits for an operation in one call.
const maxWaitPerCall = time.Minute

// An Operation is a mutation the API accepted and carries out after
// answering. Its result, of type T, is the resource it acted on, or
// [emptypb.Empty] for a delete.
//
// [emptypb.Empty]: https://pkg.go.dev/google.golang.org/protobuf/types/known/emptypb#Empty
type Operation[T proto.Message] struct {
	service *Service
	op      *longrunningpb.Operation
}

// NewOperation returns the operation op, which a method of service
// answered with. Generated clients call it.
func NewOperation[T proto.Message](service *Service, op *longrunningpb.Operation) *Operation[T] {
	return &Operation[T]{service: service, op: op}
}

// Name is the operation's resource name, by which [Operation.Wait] and
// a service's OperationClient find it.
func (o *Operation[T]) Name() string { return o.op.GetName() }

// Done reports whether the operation had finished when it was last read.
func (o *Operation[T]) Done() bool { return o.op.GetDone() }

// Proto returns the operation as it was last read, its metadata included.
func (o *Operation[T]) Proto() *longrunningpb.Operation { return o.op }

// Wait waits until the operation finishes and returns its result, or the
// [*Error] it failed with. It stops waiting, with ctx's error, when ctx
// is done. Once it returns a result, reads through the same [Client] show
// the operation's effect.
func (o *Operation[T]) Wait(ctx context.Context) (T, error) {
	for !o.op.GetDone() {
		timeout := maxWaitPerCall
		if deadline, ok := ctx.Deadline(); ok {
			timeout = min(timeout, time.Until(deadline).Truncate(time.Second))
		}
		if timeout < time.Second {
			timeout = time.Second
		}
		body, err := structpb.NewStruct(map[string]any{
			"timeout": fmt.Sprintf("%ds", int(timeout.Seconds())),
		})
		if err != nil {
			var zero T
			return zero, fmt.Errorf("origincloud: %w", err)
		}
		path, err := PathValue("operation name", o.op.GetName(), "projects/*/zones/*/operations/*")
		if err != nil {
			var zero T
			return zero, err
		}
		waited := new(longrunningpb.Operation)
		err = o.service.Call(ctx, Request{
			Method:     http.MethodPost,
			Path:       "/v1beta1/" + path + ":wait",
			Body:       body,
			Idempotent: true,
		}, waited)
		if err != nil {
			var zero T
			if ctx.Err() != nil {
				return zero, ctx.Err()
			}
			return zero, err
		}
		o.op = waited
	}
	return o.Result()
}

// Result returns the result of an operation that is done, or the [*Error]
// it failed with. It fails for an operation that isn't done yet.
func (o *Operation[T]) Result() (T, error) {
	var zero T
	if !o.op.GetDone() {
		return zero, fmt.Errorf("origincloud: operation %s isn't done", o.op.GetName())
	}
	if failure := o.op.GetError(); failure != nil {
		return zero, errorFromStatus(failure)
	}
	result := zero.ProtoReflect().New().Interface().(T)
	if err := o.op.GetResponse().UnmarshalTo(result); err != nil {
		return zero, fmt.Errorf("origincloud: reading the result of operation %s: %w", o.op.GetName(), err)
	}
	return result, nil
}
