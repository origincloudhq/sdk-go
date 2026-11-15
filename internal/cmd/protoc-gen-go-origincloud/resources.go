package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
)

// anyService is a path field of every resource service: type "*".
const anyService = "*"

// resources are the resource types the API declares in its files
// (origincloud/): each type's patterns, by type.
type resources struct {
	patterns map[string][]string
}

// indexResources returns every resource type the API's files declare, with
// google.api.resource on a message or google.api.resource_definition on a
// file; an error for a type that isn't {service}.origincloudapis.eu/{Kind}.
func indexResources(files []*protogen.File) (*resources, error) {
	r := &resources{patterns: map[string][]string{}}
	add := func(on string, d *annotations.ResourceDescriptor) error {
		if _, err := serviceOfResourceType(d.GetType()); err != nil {
			return fmt.Errorf("%s: the resource type %q: %w", on, d.GetType(), err)
		}
		r.patterns[d.GetType()] = append(r.patterns[d.GetType()], d.GetPattern()...)
		return nil
	}
	var walk func([]*protogen.Message) error
	walk = func(messages []*protogen.Message) error {
		for _, m := range messages {
			if d, _ := proto.GetExtension(m.Desc.Options(), annotations.E_Resource).(*annotations.ResourceDescriptor); d != nil {
				if err := add(string(m.Desc.FullName()), d); err != nil {
					return err
				}
			}
			if err := walk(m.Messages); err != nil {
				return err
			}
		}
		return nil
	}
	for _, f := range files {
		if !strings.HasPrefix(f.Desc.Path(), "origincloud/") {
			continue
		}
		for _, d := range proto.GetExtension(f.Desc.Options(), annotations.E_ResourceDefinition).([]*annotations.ResourceDescriptor) {
			if err := add(f.Desc.Path(), d); err != nil {
				return nil, err
			}
		}
		if err := walk(f.Messages); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// services returns the short codes of the services owning a declared
// resource type, in order.
func (r *resources) services() []string {
	var services []string
	for t := range r.patterns {
		s, _ := serviceOfResourceType(t)
		if !slices.Contains(services, s) {
			services = append(services, s)
		}
	}
	slices.Sort(services)
	return services
}

// pathServices returns the short codes of the services whose full names
// the path field field (the last of a path variable's fields) takes, in
// order: the service of the type the field refers to.
//
//   - google.api.resource_reference with type "*": every resource service,
//     anyService.
//   - With a type: that type's service.
//   - With a child_type: the services of the declared types whose pattern
//     is a pattern of the child's without its last collection and id, its
//     parent (organizations/{organization}, resourcemanager's
//     Organization). A parent no declared type has, such as
//     projects/{project}/zones/{zone}, is the child's service's.
//   - The name of a resource, a message with google.api.resource (an
//     update's resource): that resource's service.
//   - Otherwise: none, which the client reads as its own service.
func (r *resources) pathServices(field *protogen.Field) ([]string, error) {
	if ref, _ := proto.GetExtension(field.Desc.Options(), annotations.E_ResourceReference).(*annotations.ResourceReference); ref != nil {
		switch {
		case ref.GetType() == anyService:
			return []string{anyService}, nil
		case ref.GetType() != "":
			s, err := serviceOfResourceType(ref.GetType())
			if err != nil {
				return nil, fmt.Errorf("%s refers to %q: %w", field.Desc.FullName(), ref.GetType(), err)
			}
			return []string{s}, nil
		case ref.GetChildType() != "":
			child, err := serviceOfResourceType(ref.GetChildType())
			if err != nil {
				return nil, fmt.Errorf("%s refers to %q: %w", field.Desc.FullName(), ref.GetChildType(), err)
			}
			if parents := r.parentServices(ref.GetChildType()); len(parents) > 0 {
				return parents, nil
			}
			return []string{child}, nil
		}
	}
	if field.Desc.Name() == "name" && field.Parent != nil {
		if d, _ := proto.GetExtension(field.Parent.Desc.Options(), annotations.E_Resource).(*annotations.ResourceDescriptor); d != nil {
			s, err := serviceOfResourceType(d.GetType())
			if err != nil {
				return nil, fmt.Errorf("%s: the resource type %q: %w", field.Parent.Desc.FullName(), d.GetType(), err)
			}
			return []string{s}, nil
		}
	}
	return nil, nil
}

// parentServices returns the services of the declared parents of child,
// in order; none when no declared type is its parent.
func (r *resources) parentServices(child string) []string {
	var parents []string
	for _, p := range r.patterns[child] {
		segments := strings.Split(p, "/")
		if len(segments) > 2 {
			parents = append(parents, strings.Join(segments[:len(segments)-2], "/"))
		}
	}
	var services []string
	for t, patterns := range r.patterns {
		for _, p := range patterns {
			if slices.Contains(parents, p) {
				s, _ := serviceOfResourceType(t)
				if !slices.Contains(services, s) {
					services = append(services, s)
				}
			}
		}
	}
	slices.Sort(services)
	return services
}

// serviceOfResourceType returns the short code of the service owning a
// resource type, "{service}.origincloudapis.eu/{Kind}".
func serviceOfResourceType(t string) (string, error) {
	domain, kind, ok := strings.Cut(t, "/")
	if !ok {
		return "", fmt.Errorf("a resource type is {service}.origincloudapis.eu/{Kind}")
	}
	service, ok := strings.CutSuffix(domain, ".origincloudapis.eu")
	if !ok || !shortcodePattern.MatchString(service) {
		return "", fmt.Errorf("its domain isn't a service's host, {service}.origincloudapis.eu, with a short code [a-z0-9]+")
	}
	if !kindPattern.MatchString(kind) {
		return "", fmt.Errorf("its kind, %q, isn't UpperCamelCase", kind)
	}
	return service, nil
}

var (
	shortcodePattern = regexp.MustCompile(`^[a-z0-9]+$`)
	kindPattern      = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
)

// generateResourceServices writes core's list of the services that have
// resources, the domains of the resource types the API declares, such as
// pvs.origincloudapis.eu/Volume's pvs. A full resource name's host is one
// of theirs, so the list is generated, and a new service's names parse
// without anyone keeping it. It needs every file of the API in one
// request: buf runs the plugin with the strategy "all".
func generateResourceServices(gen *protogen.Plugin, r *resources) {
	services := r.services()
	if len(services) == 0 {
		return
	}
	g := gen.NewGeneratedFile(string(corePackage)+"/resource_services.pb.go", corePackage)
	g.P("// Code generated by protoc-gen-go-origincloud. DO NOT EDIT.")
	g.P()
	g.P("package core")
	g.P()
	g.P("// The services that have resources, by their short code, in order: the")
	g.P("// domains of the resource types the API declares. See [ResourceServices].")
	g.P("var resourceServices = []string{")
	for _, s := range services {
		g.P(fmt.Sprintf("%q,", s))
	}
	g.P("}")
}
