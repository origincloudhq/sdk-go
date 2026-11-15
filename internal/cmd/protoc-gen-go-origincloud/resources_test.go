package main

import (
	"slices"
	"testing"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

func resourceMessage(name, resourceType, pattern string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
	opts := &descriptorpb.MessageOptions{}
	proto.SetExtension(opts, annotations.E_Resource, &annotations.ResourceDescriptor{Type: resourceType, Pattern: []string{pattern}})
	return &descriptorpb.DescriptorProto{Name: proto.String(name), Options: opts, Field: fields}
}

func stringField(name string, number int32, ref *annotations.ResourceReference) *descriptorpb.FieldDescriptorProto {
	f := &descriptorpb.FieldDescriptorProto{
		Name: proto.String(name), Number: proto.Int32(number), JsonName: proto.String(name),
		Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
	}
	if ref != nil {
		f.Options = &descriptorpb.FieldOptions{}
		proto.SetExtension(f.Options, annotations.E_ResourceReference, ref)
	}
	return f
}

// A path field takes the full names of the service of the type it refers to.
func TestAPathFieldTakesTheNamesOfTheTypeItRefersTo(t *testing.T) {
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("origincloud/scratch/v1/scratch.proto"),
		Package: proto.String("origincloud.scratch.v1"),
		Syntax:  proto.String("proto3"),
		Options: &descriptorpb.FileOptions{GoPackage: proto.String("example.com/scratch;scratch")},
		MessageType: []*descriptorpb.DescriptorProto{
			resourceMessage("Project", "resourcemanager.origincloudapis.eu/Project", "projects/{project}", stringField("name", 1, nil)),
			resourceMessage("Zone", "location.origincloudapis.eu/Zone", "zones/{zone}"),
			resourceMessage("Volume", "pvs.origincloudapis.eu/Volume", "projects/{project}/zones/{zone}/volumes/{volume}", stringField("name", 1, nil)),
			resourceMessage("Cost", "billing.origincloudapis.eu/Cost", "projects/{project}/costs/{cost}"),
			resourceMessage("PublicImage", "compute.origincloudapis.eu/PublicImage", "zones/{zone}/images/{image}"),
			{Name: proto.String("Request"), Field: []*descriptorpb.FieldDescriptorProto{
				stringField("own", 1, &annotations.ResourceReference{Type: "pvs.origincloudapis.eu/Volume"}),
				stringField("other", 2, &annotations.ResourceReference{Type: "resourcemanager.origincloudapis.eu/Project"}),
				stringField("any", 3, &annotations.ResourceReference{Type: "*"}),
				stringField("costs", 4, &annotations.ResourceReference{ChildType: "billing.origincloudapis.eu/Cost"}),
				stringField("images", 5, &annotations.ResourceReference{ChildType: "compute.origincloudapis.eu/PublicImage"}),
				stringField("volumes", 6, &annotations.ResourceReference{ChildType: "pvs.origincloudapis.eu/Volume"}),
				stringField("plain", 7, nil),
			}},
		},
	}
	gen, err := protogen.Options{}.New(&pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{file.GetName()},
		ProtoFile:      []*descriptorpb.FileDescriptorProto{file},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := indexResources(gen.Files)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.services(); !slices.Equal(got, []string{"billing", "compute", "location", "pvs", "resourcemanager"}) {
		t.Errorf("services = %q", got)
	}
	messages := map[string]*protogen.Message{}
	for _, m := range gen.Files[0].Messages {
		messages[m.GoIdent.GoName] = m
	}
	field := func(message, name string) *protogen.Field {
		for _, f := range messages[message].Fields {
			if string(f.Desc.Name()) == name {
				return f
			}
		}
		t.Fatalf("no %s.%s", message, name)
		return nil
	}
	for _, c := range []struct {
		message, field string
		want           []string
	}{
		{"Request", "own", []string{"pvs"}},
		{"Request", "other", []string{"resourcemanager"}},
		{"Request", "any", []string{anyService}},
		// A child's parent is the declared type with its parent pattern.
		{"Request", "costs", []string{"resourcemanager"}},
		{"Request", "images", []string{"location"}},
		// projects/{project}/zones/{zone} is no declared type: the child's
		// own service.
		{"Request", "volumes", []string{"pvs"}},
		// A resource's own name.
		{"Volume", "name", []string{"pvs"}},
		{"Project", "name", []string{"resourcemanager"}},
		// Nothing to go by: the client's own service.
		{"Request", "plain", nil},
	} {
		got, err := r.pathServices(field(c.message, c.field))
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s.%s: %q, %v; want %q", c.message, c.field, got, err, c.want)
		}
	}
}
