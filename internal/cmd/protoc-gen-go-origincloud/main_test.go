package main

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestADeclaredIdempotencyLevelMakesAMethodRepeatable(t *testing.T) {
	method := func(name string, level *descriptorpb.MethodOptions_IdempotencyLevel) *descriptorpb.MethodDescriptorProto {
		m := &descriptorpb.MethodDescriptorProto{Name: proto.String(name), InputType: proto.String(".scratch.Message"), OutputType: proto.String(".scratch.Message")}
		if level != nil {
			m.Options = &descriptorpb.MethodOptions{IdempotencyLevel: level}
		}
		return m
	}
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        proto.String("scratch.proto"),
		Package:     proto.String("scratch"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Message")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Scratch"),
			Method: []*descriptorpb.MethodDescriptorProto{
				method("Wait", descriptorpb.MethodOptions_NO_SIDE_EFFECTS.Enum()),
				method("Set", descriptorpb.MethodOptions_IDEMPOTENT.Enum()),
				method("Change", nil),
			},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	methods := file.Services().Get(0).Methods()
	for name, want := range map[string]bool{"Wait": true, "Set": true, "Change": false} {
		if got := declaredIdempotent(methods.ByName(protoreflect.Name(name))); got != want {
			t.Errorf("%s: declaredIdempotent = %v, want %v", name, got, want)
		}
	}
}

func TestTheGlobalServicesHaveNoRegionalHost(t *testing.T) {
	for shortcode, global := range map[string]bool{
		"iam": true, "resourcemanager": true, "location": true,
		"pvs": false, "compute": false, "network": false,
	} {
		if globalServices[shortcode] != global {
			t.Errorf("%s: global = %v, want %v", shortcode, globalServices[shortcode], global)
		}
	}
}
