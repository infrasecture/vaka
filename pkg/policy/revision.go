package policy

import (
	"crypto/sha256"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Revision returns a stable digest of the effective policy that affects a
// service container. GeneratedBy is diagnostic metadata and deliberately does
// not participate, so a CLI-only release does not recreate containers.
func Revision(p *ServicePolicy) (string, error) {
	if p == nil {
		return "", fmt.Errorf("compute policy revision: nil policy")
	}
	semantic := *p
	semantic.GeneratedBy = ""
	raw, err := yaml.Marshal(&semantic)
	if err != nil {
		return "", fmt.Errorf("marshal policy revision input: %w", err)
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%x", sum), nil
}
