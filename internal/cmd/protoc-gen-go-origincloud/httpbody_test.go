package main

import (
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/pluginpb"
)

// generateScratch runs the generator on a file of one service with method,
// whose requests are scratch.Request: a name, and a file.
func generateScratch(t *testing.T, method *descriptorpb.MethodDescriptorProto) (string, error) {
	t.Helper()
	scratch := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("scratch.proto"),
		Package:    proto.String("scratch"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"google/api/annotations.proto", "google/api/httpbody.proto"},
		Options:    &descriptorpb.FileOptions{GoPackage: proto.String("example.com/scratch;scratch")},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Request"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("name"), JsonName: proto.String("name"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()},
				{Name: proto.String("file"), JsonName: proto.String("file"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".google.api.HttpBody"), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()},
			},
		}},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("FileService"), Method: []*descriptorpb.MethodDescriptorProto{method}}},
	}
	var files []*descriptorpb.FileDescriptorProto
	for _, f := range []protoreflect.FileDescriptor{
		descriptorpb.File_google_protobuf_descriptor_proto,
		anypb.File_google_protobuf_any_proto,
		annotations.File_google_api_http_proto,
		annotations.File_google_api_annotations_proto,
		httpbody.File_google_api_httpbody_proto,
	} {
		files = append(files, protodesc.ToFileDescriptorProto(f))
	}
	gen, err := protogen.Options{}.New(&pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{"scratch.proto"},
		ProtoFile:      append(files, scratch),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := generate(gen); err != nil {
		return "", err
	}
	response := gen.Response()
	if response.GetError() != "" {
		t.Fatal(response.GetError())
	}
	if len(response.GetFile()) != 1 {
		t.Fatalf("generated %d files, want one", len(response.GetFile()))
	}
	return response.GetFile()[0].GetContent(), nil
}

func scratchMethod(name, output string, rule *annotations.HttpRule) *descriptorpb.MethodDescriptorProto {
	options := &descriptorpb.MethodOptions{}
	proto.SetExtension(options, annotations.E_Http, rule)
	return &descriptorpb.MethodDescriptorProto{Name: proto.String(name), InputType: proto.String(".scratch.Request"), OutputType: proto.String(output), Options: options}
}

func TestAMethodAnsweringWithAnHttpBodyReturnsTheFileAsItIs(t *testing.T) {
	code, err := generateScratch(t, scratchMethod("DownloadFile", ".google.api.HttpBody",
		&annotations.HttpRule{Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=files/*}:download"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(code, "return c.service.CallHTTPBody(ctx, core.Request{") {
		t.Errorf("the method doesn't return the file as it is:\n%s", code)
	}
	if strings.Contains(code, "c.service.Call(ctx") {
		t.Errorf("the method reads the file as JSON:\n%s", code)
	}
}

func TestARequestBodyThatIsAnHttpBodyIsRefused(t *testing.T) {
	_, err := generateScratch(t, scratchMethod("UploadFile", ".google.api.HttpBody",
		&annotations.HttpRule{Pattern: &annotations.HttpRule_Post{Post: "/v1/{name=files/*}:upload"}, Body: "file"}))
	if err == nil || !strings.Contains(err.Error(), "google.api.HttpBody isn't supported") {
		t.Fatalf("err = %v, want the request body refused", err)
	}
}
