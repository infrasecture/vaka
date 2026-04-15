// pkg/policy/parse.go
package policy

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Parse reads a ServicePolicy document from r.
// Unknown YAML fields are a hard error. defaultAction defaults to "reject".
func Parse(r io.Reader) (*ServicePolicy, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var p ServicePolicy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse vaka.yaml: %w", err)
	}

	// Apply defaultAction default.
	for name, svc := range p.Services {
		if svc == nil {
			return nil, fmt.Errorf("services.%s: nil service config", name)
		}
		if svc.Network != nil && svc.Network.Egress != nil {
			if svc.Network.Egress.DefaultAction == "" {
				svc.Network.Egress.DefaultAction = "reject"
			}
		}
	}

	return &p, nil
}

// SliceService returns a new ServicePolicy containing only the named service.
// The APIVersion and Kind fields are preserved. Panics if service not found.
func SliceService(p *ServicePolicy, name string) *ServicePolicy {
	svc, ok := p.Services[name]
	if !ok {
		panic(fmt.Sprintf("SliceService: service %q not found", name))
	}
	return &ServicePolicy{
		APIVersion: p.APIVersion,
		Kind:       p.Kind,
		Services:   map[string]*ServiceConfig{name: svc},
	}
}
