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
		"iam": true, "resourcemanager": true, "location": true, "billing": true, "support": true, "dns": true,
		"pvs": false, "compute": false, "network": false, "observability": false,
	} {
		if globalServices[shortcode] != global {
			t.Errorf("%s: global = %v, want %v", shortcode, globalServices[shortcode], global)
		}
	}
}

func TestOnlyTheSecurityTokenServiceIsUnauthenticated(t *testing.T) {
	for shortcode, unauthenticated := range map[string]bool{
		"sts": true, "iam": false, "resourcemanager": false, "pvs": false, "compute": false,
	} {
		if unauthenticatedServices[shortcode] != unauthenticated {
			t.Errorf("%s: unauthenticated = %v, want %v", shortcode, unauthenticatedServices[shortcode], unauthenticated)
		}
	}
}

func TestAResourceTypeNamesItsService(t *testing.T) {
	for resourceType, want := range map[string]string{
		"pvs.origincloudapis.eu/Volume":              "pvs",
		"resourcemanager.origincloudapis.eu/Project": "resourcemanager",
		"kms.origincloudapis.eu/KeyVersion":          "kms",
		"pvs.example.com/Volume":                     "",
		"origincloudapis.eu/Volume":                  "",
		"pvs.eu-central1.origincloudapis.eu/Volume":  "",
		"pvs.origincloudapis.eu/volume":              "",
		"pvs.origincloudapis.eu":                     "",
		"pvs.origincloudapis.eu/Volume/x":            "",
		"PVS.origincloudapis.eu/Volume":              "",
	} {
		got, err := serviceOfResourceType(resourceType)
		if want == "" && err == nil {
			t.Errorf("%s: %q, want an error", resourceType, got)
		}
		if want != "" && (err != nil || got != want) {
			t.Errorf("%s: %q, %v; want %q", resourceType, got, err, want)
		}
	}
}
