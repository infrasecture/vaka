# vaka Secure Container Layer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Historical note (2026-04-20+):** this archived implementation plan contains
> references to the now-removed `runtime.runAs` field. Current behavior uses
> generated `services.<name>.user` for service identity restoration.

**Goal:** Build `vaka-init` (container init binary) + `vaka` (host CLI) that enforce nftables egress firewall rules inside Docker containers running AI agentic harnesses, with zero changes to the original docker-compose.yaml.

**Architecture:** `vaka-init` is a static Go binary in harness images that reads a per-service `ServicePolicy` from a Docker secret, applies nftables rules atomically via `nft -f /dev/stdin`, drops Linux capabilities, optionally changes UID/GID, then `execve`s the harness. `vaka` is a host CLI that parses `vaka.yaml`, validates it strictly, generates a compose override piped via stdin, injects policy as env-var-backed Docker secrets, rewrites service entrypoints to `vaka-init`, and proxies `docker compose up`/`run`. The binaries communicate only through the Docker secrets mechanism — no temp files, no disk artifacts.

**Tech Stack:** Go 1.23, `gopkg.in/yaml.v3`, `golang.org/x/sys/unix`, `github.com/syndtr/gocapability/capability`, `github.com/docker/docker/client`, `github.com/spf13/cobra`, Go `text/template` + `embed.FS`

---

## File Structure

| File | Responsibility |
|---|---|
| `go.mod` | Module `vaka.dev/vaka`; all dependencies pinned |
| `go.work` | Go workspace — single module |
| `pkg/policy/types.go` | All config schema types; custom `PortSpec` and `ICMPSpec` YAML unmarshalers |
| `pkg/policy/parse.go` | Strict YAML parsing (unknown fields = hard error); defaults `defaultAction` to `reject`; per-service slice |
| `pkg/policy/parse_test.go` | Parse tests |
| `pkg/policy/validate.go` | All §6.3 validation rules; accumulates all errors before returning |
| `pkg/policy/validate_test.go` | Validation tests |
| `pkg/nft/types.go` | `RulesetData` struct passed to the nft template |
| `pkg/nft/templates/egress.nft.tmpl` | Go `text/template` for nft DSL; embedded via `embed.FS` |
| `pkg/nft/generate.go` | Policy → `RulesetData` → rendered ruleset string; handles IPv4/IPv6 family split; stub comments for unresolved names |
| `pkg/nft/generate_test.go` | Rule generation tests |
| `pkg/nft/resolve.go` | Parses `/etc/resolv.conf`; resolves A+AAAA records for hostnames and `dns: {}` |
| `pkg/nft/resolve_test.go` | Resolve tests (fake resolv.conf; `net.DefaultResolver` interface) |
| `pkg/compose/override.go` | Typed compose override structs; `yaml.Marshal()` output; per-service secret mounts and cap delta |
| `pkg/compose/override_test.go` | Override YAML shape tests |
| `cmd/vaka-init/main.go` | Container init: parse secret → resolve → generate → `nft -f /dev/stdin` → drop caps → setresgid/setresuid → `execve` |
| `cmd/vaka/main.go` | Root cobra command + `version` subcommand |
| `cmd/vaka/validate.go` | `vaka validate`: parse vaka.yaml + docker-compose.yaml, run validation, print summary |
| `cmd/vaka/show.go` | `vaka show <service>`: parse + validate, render nft preview without DNS resolution |
| `cmd/vaka/inject.go` | `extractVakaFlags`, `findSubcmd`, `discoverComposeFiles`, `injectStdinOverride`: all argv manipulation for injection path |
| `cmd/vaka/inject_test.go` | Table-driven tests for injection algorithm: last-`-f`, no-`-f`, `--file=value`, `--` boundary; `TestFindSubcmd` |
| `cmd/vaka/up.go` | `runInjection(vakaFile, args)`: load+validate, compose-go multi-file merge, entrypoint/cap resolution, compose override, inject `-f -` into argv; `resolveEntrypoint`, `computeCapDelta` |
| `docker/init/Dockerfile` | Multi-stage: `emsi/nft-static` + Go builder → `scratch` image with exactly two binaries |

---

### Task 1: Go Module Scaffolding

**Files:**
- Create: `go.mod`
- Create: `go.work`

- [ ] **Step 1: Initialise the module**

Run from the repo root (`/home/emsi/git/vaka`):

```bash
go mod init vaka.dev/vaka
go work init .
```

- [ ] **Step 2: Add all dependencies**

```bash
go get gopkg.in/yaml.v3@v3.0.1
go get golang.org/x/sys@latest
go get github.com/syndtr/gocapability@latest
go get github.com/docker/docker@latest
go get github.com/spf13/cobra@latest
go get github.com/compose-spec/compose-go/v2@latest
go mod tidy
```

- [ ] **Step 3: Verify go.mod has the correct module path**

`go.mod` must start with:

```
module vaka.dev/vaka

go 1.23
```

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum go.work
git commit -m "chore: initialise Go module vaka.dev/vaka"
```

---

### Task 2: Policy Types

**Files:**
- Create: `pkg/policy/types.go`

- [ ] **Step 1: Write the types**

```go
// pkg/policy/types.go
package policy

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServicePolicy is the top-level config document (vaka.yaml).
type ServicePolicy struct {
	APIVersion string                    `yaml:"apiVersion"`
	Kind       string                    `yaml:"kind"`
	Services   map[string]*ServiceConfig `yaml:"services"`
}

// ServiceConfig holds per-service network and runtime policy.
type ServiceConfig struct {
	Network *NetworkConfig `yaml:"network,omitempty"`
	Runtime *RuntimeConfig `yaml:"runtime,omitempty"`
}

// NetworkConfig wraps the egress policy.
type NetworkConfig struct {
	Egress *EgressPolicy `yaml:"egress,omitempty"`
}

// EgressPolicy defines allowed/denied outbound traffic for one service.
type EgressPolicy struct {
	DefaultAction string `yaml:"defaultAction,omitempty"`
	Accept        []Rule `yaml:"accept,omitempty"`
	Reject        []Rule `yaml:"reject,omitempty"`
	Drop          []Rule `yaml:"drop,omitempty"`
	BlockMetadata bool   `yaml:"block_metadata,omitempty"`
}

// Rule is one entry in an accept/reject/drop list.
// Exactly one of DNS or Proto/To/Ports/Type should be set.
type Rule struct {
	DNS   *DNSRule   `yaml:"dns,omitempty"`
	Proto string     `yaml:"proto,omitempty"`
	To    []string   `yaml:"to,omitempty"`
	Ports []PortSpec `yaml:"ports,omitempty"`
	Type  *ICMPSpec  `yaml:"type,omitempty"`
}

// DNSRule is the dns: {} shorthand. Servers overrides resolv.conf if set.
type DNSRule struct {
	Servers []string `yaml:"servers,omitempty"`
}

// PortSpec holds a single port (Single > 0, IsRange == false)
// or a range (IsRange == true, RangeStart, RangeEnd set).
type PortSpec struct {
	Single     int
	RangeStart int
	RangeEnd   int
	IsRange    bool
}

// UnmarshalYAML handles both integer and "N-M" string forms.
func (p *PortSpec) UnmarshalYAML(value *yaml.Node) error {
	// Try integer first.
	var single int
	if err := value.Decode(&single); err == nil {
		if single < 1 || single > 65535 {
			return fmt.Errorf("port %d out of range (1–65535)", single)
		}
		p.Single = single
		return nil
	}
	// Try "N-M" string.
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("port must be an integer or a range string \"N-M\"")
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid port range %q: expected \"N-M\"", s)
	}
	start, err1 := strconv.Atoi(parts[0])
	end, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return fmt.Errorf("invalid port range %q: both values must be integers", s)
	}
	if start < 1 || end > 65535 || start >= end {
		return fmt.Errorf("invalid port range %q: values must be 1–65535 and start < end", s)
	}
	p.RangeStart = start
	p.RangeEnd = end
	p.IsRange = true
	return nil
}

// NftString returns the nft representation of this port spec.
func (p PortSpec) NftString() string {
	if p.IsRange {
		return fmt.Sprintf("%d-%d", p.RangeStart, p.RangeEnd)
	}
	return strconv.Itoa(p.Single)
}

// ICMPSpec holds an ICMP type as either a named string or an integer.
type ICMPSpec struct {
	Name  string
	Num   int
	IsNum bool
}

// UnmarshalYAML handles both string names and integer type codes.
func (i *ICMPSpec) UnmarshalYAML(value *yaml.Node) error {
	// YAML integers arrive as !!int nodes.
	var n int
	if err := value.Decode(&n); err == nil {
		if n < 0 || n > 255 {
			return fmt.Errorf("ICMP type %d out of range (0–255)", n)
		}
		i.Num = n
		i.IsNum = true
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("ICMP type must be a string name or integer 0–255")
	}
	// Numeric string (e.g. type: "8").
	if parsed, err := strconv.Atoi(s); err == nil {
		if parsed < 0 || parsed > 255 {
			return fmt.Errorf("ICMP type %d out of range (0–255)", parsed)
		}
		i.Num = parsed
		i.IsNum = true
		return nil
	}
	i.Name = s
	return nil
}

// NftString returns the nft-ready type token.
func (i *ICMPSpec) NftString() string {
	if i.IsNum {
		return strconv.Itoa(i.Num)
	}
	return i.Name
}

// RuntimeConfig holds capability and identity settings for vaka-init.
type RuntimeConfig struct {
	DropCaps []string   `yaml:"dropCaps,omitempty"`
	RunAs    *RunAsConfig `yaml:"runAs,omitempty"`
}

// RunAsConfig specifies the UID/GID to switch to after firewall setup.
type RunAsConfig struct {
	UID int `yaml:"uid"`
	GID int `yaml:"gid"`
}
```

- [ ] **Step 2: Verify the package compiles**

```bash
go build ./pkg/policy/...
```

Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add pkg/policy/types.go
git commit -m "feat(policy): add ServicePolicy schema types with PortSpec/ICMPSpec unmarshalers"
```

---

### Task 3: Policy Parsing

**Files:**
- Create: `pkg/policy/parse.go`
- Create: `pkg/policy/parse_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// pkg/policy/parse_test.go
package policy_test

import (
	"strings"
	"testing"

	"vaka.dev/vaka/pkg/policy"
)

const minimalValid = `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  codex:
    network:
      egress:
        defaultAction: reject
        accept:
          - proto: tcp
            to: [10.0.0.1]
            ports: [443]
`

func TestParseMinimalValid(t *testing.T) {
	p, err := policy.Parse(strings.NewReader(minimalValid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.APIVersion != "vaka.dev/v1alpha1" {
		t.Errorf("APIVersion = %q, want %q", p.APIVersion, "vaka.dev/v1alpha1")
	}
	if p.Kind != "ServicePolicy" {
		t.Errorf("Kind = %q, want %q", p.Kind, "ServicePolicy")
	}
	svc, ok := p.Services["codex"]
	if !ok {
		t.Fatal("services.codex not found")
	}
	if svc.Network.Egress.DefaultAction != "reject" {
		t.Errorf("defaultAction = %q, want %q", svc.Network.Egress.DefaultAction, "reject")
	}
}

func TestParseUnknownFieldRejected(t *testing.T) {
	input := `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  codex:
    network:
      egress:
        defaultAction: reject
        typo_field: bad
`
	_, err := policy.Parse(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "typo_field") {
		t.Errorf("error %q does not mention the unknown field", err.Error())
	}
}

func TestParseDefaultActionDefaultsToReject(t *testing.T) {
	input := `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  codex:
    network:
      egress:
        accept:
          - proto: tcp
            to: [10.0.0.1]
            ports: [443]
`
	p, err := policy.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Services["codex"].Network.Egress.DefaultAction != "reject" {
		t.Errorf("defaultAction = %q, want %q (default)",
			p.Services["codex"].Network.Egress.DefaultAction, "reject")
	}
}

func TestParsePortRange(t *testing.T) {
	input := `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  svc:
    network:
      egress:
        accept:
          - proto: tcp
            to: [10.0.0.1]
            ports: [443, "8080-8090"]
`
	p, err := policy.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ports := p.Services["svc"].Network.Egress.Accept[0].Ports
	if len(ports) != 2 {
		t.Fatalf("len(ports) = %d, want 2", len(ports))
	}
	if ports[0].Single != 443 {
		t.Errorf("ports[0].Single = %d, want 443", ports[0].Single)
	}
	if !ports[1].IsRange || ports[1].RangeStart != 8080 || ports[1].RangeEnd != 8090 {
		t.Errorf("ports[1] = %+v, want IsRange=true RangeStart=8080 RangeEnd=8090", ports[1])
	}
}

func TestParseSliceSingleService(t *testing.T) {
	p, _ := policy.Parse(strings.NewReader(minimalValid))
	sliced := policy.SliceService(p, "codex")
	if len(sliced.Services) != 1 {
		t.Errorf("len(services) = %d, want 1", len(sliced.Services))
	}
	if _, ok := sliced.Services["codex"]; !ok {
		t.Error("expected service 'codex' in sliced policy")
	}
}
```

- [ ] **Step 2: Run tests — confirm they fail**

```bash
go test ./pkg/policy/... -run 'TestParse' -v 2>&1 | head -20
```

Expected: compile error — `policy.Parse` and `policy.SliceService` are undefined.

- [ ] **Step 3: Implement parse.go**

```go
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
```

- [ ] **Step 4: Run tests — confirm they pass**

```bash
go test ./pkg/policy/... -run 'TestParse' -v
```

Expected: all `TestParse*` tests PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/policy/parse.go pkg/policy/parse_test.go
git commit -m "feat(policy): strict YAML parsing with defaultAction default and service slice"
```

---

### Task 4: Policy Validation

**Files:**
- Create: `pkg/policy/validate.go`
- Create: `pkg/policy/validate_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// pkg/policy/validate_test.go
package policy_test

import (
	"strings"
	"testing"

	"vaka.dev/vaka/pkg/policy"
)

func mustParse(t *testing.T, s string) *policy.ServicePolicy {
	t.Helper()
	p, err := policy.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestValidateAPIVersion(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v2
kind: ServicePolicy
services:
  s: {}
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for wrong apiVersion")
	}
	if !strings.Contains(errs[0].Error(), "apiVersion") {
		t.Errorf("error %q does not mention apiVersion", errs[0])
	}
}

func TestValidateKind(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: BadKind
services:
  s: {}
`)
	errs := policy.Validate(p, nil)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "kind") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected error mentioning 'kind', got: %v", errs)
	}
}

func TestValidateUnknownProto(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        accept:
          - proto: udpp
            to: [10.0.0.1]
            ports: [53]
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for unknown proto")
	}
	if !strings.Contains(errs[0].Error(), "udpp") {
		t.Errorf("error %q does not mention 'udpp'", errs[0])
	}
}

func TestValidatePortOutOfRange(t *testing.T) {
	// PortSpec.UnmarshalYAML catches this at parse time, but validate checks it too.
	p := &policy.ServicePolicy{
		APIVersion: "vaka.dev/v1alpha1",
		Kind:       "ServicePolicy",
		Services: map[string]*policy.ServiceConfig{
			"s": {Network: &policy.NetworkConfig{Egress: &policy.EgressPolicy{
				DefaultAction: "reject",
				Accept: []policy.Rule{{
					Proto: "tcp",
					To:    []string{"10.0.0.1"},
					Ports: []policy.PortSpec{{Single: 0}}, // 0 is invalid
				}},
			}}},
		},
	}
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for port 0")
	}
}

func TestValidateNetworkModeHost(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
`)
	compose := map[string]string{"s": "host"} // service → network_mode
	errs := policy.Validate(p, compose)
	if len(errs) == 0 {
		t.Fatal("expected error for network_mode: host")
	}
	if !strings.Contains(errs[0].Error(), "network_mode: host") {
		t.Errorf("error %q does not mention network_mode: host", errs[0])
	}
}

func TestValidateServiceNotInCompose(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  ghost:
    network:
      egress:
        defaultAction: reject
`)
	// networkModes represents the compose file — "ghost" is absent.
	errs := policy.Validate(p, map[string]string{"other": ""})
	if len(errs) == 0 {
		t.Fatal("expected error for service not in docker-compose.yaml")
	}
	if !strings.Contains(errs[0].Error(), "ghost") {
		t.Errorf("error %q does not mention service name 'ghost'", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "docker-compose.yaml") {
		t.Errorf("error %q does not mention docker-compose.yaml", errs[0])
	}
}

func TestValidateInvalidServiceName(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  "My Service!!":
    network:
      egress:
        defaultAction: reject
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for invalid service name")
	}
	if !strings.Contains(errs[0].Error(), "invalid service name") {
		t.Errorf("error %q does not mention 'invalid service name'", errs[0])
	}
}

func TestValidateValidPolicyPassesClean(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  codex:
    network:
      egress:
        defaultAction: reject
        accept:
          - dns: {}
          - proto: tcp
            to: [10.0.0.1, 10.20.0.0/16]
            ports: [443]
`)
	errs := policy.Validate(p, map[string]string{"codex": ""})
	if len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
}

func TestValidateDefaultActionAcceptIsAllowed(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: accept
`)
	// accept is valid (with a warning printed by the CLI) — validate must NOT error.
	errs := policy.Validate(p, nil)
	if len(errs) != 0 {
		t.Errorf("expected no errors for defaultAction: accept, got: %v", errs)
	}
}

func TestValidateInvalidHostname(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        accept:
          - proto: tcp
            to: ["not a valid hostname!!"]
            ports: [443]
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for invalid hostname")
	}
}

func TestValidatePortsWithoutProtoIsError(t *testing.T) {
	p := &policy.ServicePolicy{
		APIVersion: "vaka.dev/v1alpha1",
		Kind:       "ServicePolicy",
		Services: map[string]*policy.ServiceConfig{
			"s": {Network: &policy.NetworkConfig{Egress: &policy.EgressPolicy{
				DefaultAction: "reject",
				Accept: []policy.Rule{{
					// No proto — ports alone are ambiguous (TCP? UDP?).
					To:    []string{"10.0.0.1"},
					Ports: []policy.PortSpec{{Single: 443}},
				}},
			}}},
		},
	}
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for ports without proto")
	}
	if !strings.Contains(errs[0].Error(), "proto") {
		t.Errorf("error %q does not mention 'proto'", errs[0])
	}
}

func TestValidateDropCapsUnknownNameIsError(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    runtime:
      dropCaps: [NET_ADMON]
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for unknown capability name")
	}
	if !strings.Contains(errs[0].Error(), "NET_ADMON") {
		t.Errorf("error %q does not mention the bad capability name", errs[0])
	}
}

func TestValidateDropCapsCapPrefixAcceptedAndStripped(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    runtime:
      dropCaps: [CAP_NET_ADMIN, SYS_PTRACE]
`)
	errs := policy.Validate(p, nil)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
	// After validation, CAP_ prefix must be stripped.
	svc := p.Services["s"]
	if svc.Runtime.DropCaps[0] != "NET_ADMIN" {
		t.Errorf("expected CAP_ prefix stripped, got %q", svc.Runtime.DropCaps[0])
	}
	if svc.Runtime.DropCaps[1] != "SYS_PTRACE" {
		t.Errorf("expected SYS_PTRACE unchanged, got %q", svc.Runtime.DropCaps[1])
	}
}
```

- [ ] **Step 2: Run tests — confirm they fail**

```bash
go test ./pkg/policy/... -run 'TestValidate' -v 2>&1 | head -20
```

Expected: compile error — `policy.Validate` is undefined.

- [ ] **Step 3: Implement validate.go**

```go
// pkg/policy/validate.go
package policy

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

var (
	validProtos    = map[string]bool{"tcp": true, "udp": true, "icmp": true, "icmpv6": true}
	validActions   = map[string]bool{"accept": true, "reject": true, "drop": true}
	hostnameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9\-]*[a-z0-9])?)*$`)
	// svcNameRegexp matches valid Docker Compose service names (DNS label + underscore).
	svcNameRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

	// knownLinuxCaps is the set of short-form capability names defined in
	// include/uapi/linux/capability.h through Linux 5.x / 6.x.
	// Short-form means without the "CAP_" prefix.
	knownLinuxCaps = map[string]bool{
		"CHOWN": true, "DAC_OVERRIDE": true, "DAC_READ_SEARCH": true,
		"FOWNER": true, "FSETID": true, "KILL": true, "SETGID": true,
		"SETUID": true, "SETPCAP": true, "LINUX_IMMUTABLE": true,
		"NET_BIND_SERVICE": true, "NET_BROADCAST": true, "NET_ADMIN": true,
		"NET_RAW": true, "IPC_LOCK": true, "IPC_OWNER": true,
		"SYS_MODULE": true, "SYS_RAWIO": true, "SYS_CHROOT": true,
		"SYS_PTRACE": true, "SYS_PACCT": true, "SYS_ADMIN": true,
		"SYS_BOOT": true, "SYS_NICE": true, "SYS_RESOURCE": true,
		"SYS_TIME": true, "SYS_TTY_CONFIG": true, "MKNOD": true,
		"LEASE": true, "AUDIT_WRITE": true, "AUDIT_CONTROL": true,
		"SETFCAP": true, "MAC_OVERRIDE": true, "MAC_ADMIN": true,
		"SYSLOG": true, "WAKE_ALARM": true, "BLOCK_SUSPEND": true,
		"AUDIT_READ": true, "PERFMON": true, "BPF": true,
		"CHECKPOINT_RESTORE": true,
	}
)

// Validate checks p against all §6.3 rules.
// networkModes maps service name → network_mode for every service declared in
// docker-compose.yaml (pass nil to skip compose cross-checks in unit tests).
// Returns all validation errors found (not just the first).
func Validate(p *ServicePolicy, networkModes map[string]string) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if p.APIVersion != "vaka.dev/v1alpha1" {
		add("apiVersion: must be \"vaka.dev/v1alpha1\", got %q", p.APIVersion)
	}
	if p.Kind != "ServicePolicy" {
		add("kind: must be \"ServicePolicy\", got %q", p.Kind)
	}

	for name, svc := range p.Services {
		prefix := "services." + name

		// Service name must be a valid DNS label (Docker Compose naming rules).
		if !svcNameRegexp.MatchString(name) {
			add("%s: invalid service name %q (must match [a-z0-9][a-z0-9_-]*)", prefix, name)
		}

		// Service must exist in docker-compose.yaml when compose data is available.
		if networkModes != nil {
			mode, ok := networkModes[name]
			if !ok {
				add("%s: service %q not found in docker-compose.yaml", prefix, name)
			} else if mode == "host" {
				add("%s: network_mode: host — vaka cannot isolate a container sharing the host network namespace", prefix)
			}
		}

		if svc == nil || svc.Network == nil || svc.Network.Egress == nil {
			continue
		}
		e := svc.Network.Egress
		ep := prefix + ".network.egress"

		// defaultAction
		if e.DefaultAction != "" && !validActions[e.DefaultAction] {
			add("%s.defaultAction: unknown value %q (expected accept, reject, drop)", ep, e.DefaultAction)
		}

		// Validate all rule lists.
		for listName, rules := range map[string][]Rule{
			"accept": e.Accept,
			"reject": e.Reject,
			"drop":   e.Drop,
		} {
			for i, rule := range rules {
				rp := fmt.Sprintf("%s.%s[%d]", ep, listName, i)
				errs = append(errs, validateRule(rp, rule)...)
			}
		}

		// runtime
		if svc.Runtime != nil {
			rp := prefix + ".runtime"
			if svc.Runtime.RunAs != nil {
				if svc.Runtime.RunAs.UID < 0 {
					add("%s.runAs.uid: must be >= 0", rp)
				}
				if svc.Runtime.RunAs.GID < 0 {
					add("%s.runAs.gid: must be >= 0", rp)
				}
			}
			for i, cap := range svc.Runtime.DropCaps {
				// Strip CAP_ prefix (accepted per spec; normalise in place).
				short := strings.TrimPrefix(cap, "CAP_")
				svc.Runtime.DropCaps[i] = short
				if !knownLinuxCaps[short] {
					add("%s.dropCaps[%d]: unknown capability %q", rp, i, cap)
				}
			}
		}
	}

	return errs
}

func validateRule(prefix string, r Rule) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if r.DNS != nil {
		// dns: {} is always valid syntactically; nothing else to check here.
		return nil
	}

	if r.Proto != "" && !validProtos[r.Proto] {
		add("%s.proto: unknown value %q (expected tcp, udp, icmp, icmpv6)", prefix, r.Proto)
	}

	// Ports without proto are ambiguous — the kernel needs a transport protocol
	// to interpret port numbers. Require proto whenever ports are set.
	if len(r.Ports) > 0 && r.Proto == "" {
		add("%s: ports specified without proto (proto: tcp or proto: udp is required when ports are set)", prefix)
	}

	for j, entry := range r.To {
		ep := fmt.Sprintf("%s.to[%d]", prefix, j)
		if err := validateToEntry(entry); err != nil {
			add("%s: %v", ep, err)
		}
	}

	for j, port := range r.Ports {
		ep := fmt.Sprintf("%s.ports[%d]", prefix, j)
		if !port.IsRange {
			if port.Single < 1 || port.Single > 65535 {
				add("%s: port %d out of range (1–65535)", ep, port.Single)
			}
		} else {
			if port.RangeStart < 1 || port.RangeEnd > 65535 || port.RangeStart >= port.RangeEnd {
				add("%s: port range %d-%d invalid", ep, port.RangeStart, port.RangeEnd)
			}
		}
	}

	if r.Type != nil && !r.Type.IsNum {
		// Named ICMP types — validate against known nft names.
		if !knownICMPType(r.Type.Name) {
			add("%s.type: unknown ICMP type name %q", prefix, r.Type.Name)
		}
	}

	return errs
}

func validateToEntry(s string) error {
	// Valid IPv4/IPv6 address.
	if ip := net.ParseIP(s); ip != nil {
		return nil
	}
	// Valid CIDR.
	if _, _, err := net.ParseCIDR(s); err == nil {
		return nil
	}
	// Hostname.
	if hostnameRegexp.MatchString(strings.ToLower(s)) {
		return nil
	}
	return fmt.Errorf("invalid address, CIDR, or hostname: %q", s)
}

// knownICMPType returns true if name is a valid nft ICMP type keyword.
func knownICMPType(name string) bool {
	known := map[string]bool{
		"echo-reply":              true,
		"echo-request":            true,
		"destination-unreachable": true,
		"parameter-problem":       true,
		"redirect":                true,
		"router-advertisement":    true,
		"router-solicitation":     true,
		"time-exceeded":           true,
		"timestamp-reply":         true,
		"timestamp-request":       true,
		// ICMPv6
		"nd-neighbor-solicit":    true,
		"nd-neighbor-advert":     true,
		"nd-router-solicit":      true,
		"nd-router-advert":       true,
		"mld-listener-query":     true,
		"mld-listener-report":    true,
	}
	return known[name]
}
```

- [ ] **Step 4: Run tests — confirm they pass**

```bash
go test ./pkg/policy/... -v
```

Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/policy/validate.go pkg/policy/validate_test.go
git commit -m "feat(policy): host-side validation with all §6.3 rules"
```

---

### Task 5: nft Template and RulesetData

**Files:**
- Create: `pkg/nft/types.go`
- Create: `pkg/nft/templates/egress.nft.tmpl`

- [ ] **Step 1: Write types.go**

```go
// pkg/nft/types.go
package nft

// RulesetData is the data passed to egress.nft.tmpl.
// All rule strings are pre-rendered by the generator; the template only
// formats them.
type RulesetData struct {
	BlockMetadata  bool
	MetadataRanges []string // e.g. "ip  daddr 169.254.169.254/32"
	DropRules      []string
	RejectRules    []string
	AcceptRules    []string
	DefaultVerdict string // e.g. "reject with icmpx type port-unreachable"
}
```

- [ ] **Step 2: Write the template**

Create `pkg/nft/templates/egress.nft.tmpl`:

```
table inet vaka {
  chain egress {
    type filter hook output priority 0;
    policy accept;

    # implicit invariants
    ct state established,related accept
    oif "lo" accept
{{- if .BlockMetadata }}

    # metadata endpoint block (block_metadata: true)
{{- range .MetadataRanges }}
    {{ . }} drop
{{- end }}
{{- end }}
{{- if .DropRules }}

    # explicit drop rules
{{- range .DropRules }}
    {{ . }}
{{- end }}
{{- end }}
{{- if .RejectRules }}

    # explicit reject rules
{{- range .RejectRules }}
    {{ . }}
{{- end }}
{{- end }}
{{- if .AcceptRules }}

    # explicit accept rules
{{- range .AcceptRules }}
    {{ . }}
{{- end }}
{{- end }}

    # default action
    {{ .DefaultVerdict }}
  }
}
```

- [ ] **Step 3: Verify the package compiles**

```bash
go build ./pkg/nft/...
```

Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
git add pkg/nft/types.go pkg/nft/templates/egress.nft.tmpl
git commit -m "feat(nft): add RulesetData struct and egress.nft.tmpl template"
```

---

### Task 6: nft Rule Generation

**Files:**
- Create: `pkg/nft/generate.go`
- Create: `pkg/nft/generate_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// pkg/nft/generate_test.go
package nft_test

import (
	"strings"
	"testing"

	"vaka.dev/vaka/pkg/nft"
	"vaka.dev/vaka/pkg/policy"
)

func egressWithAccept(rules ...policy.Rule) *policy.EgressPolicy {
	return &policy.EgressPolicy{
		DefaultAction: "reject",
		Accept:        rules,
	}
}

func TestGenerateImplicitInvariantsAlwaysFirst(t *testing.T) {
	out, err := nft.Generate(&policy.EgressPolicy{DefaultAction: "reject"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	estIdx := strings.Index(out, "ct state established,related accept")
	loIdx := strings.Index(out, "oif \"lo\" accept")
	if estIdx < 0 {
		t.Error("established,related rule missing")
	}
	if loIdx < 0 {
		t.Error("oif lo rule missing")
	}
	if estIdx > loIdx {
		t.Error("established,related must come before oif lo")
	}
}

func TestGenerateTCPRuleIPv4(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"192.168.1.10"},
		Ports: []policy.PortSpec{{Single: 443}},
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := "ip  daddr { 192.168.1.10 } tcp dport { 443 } accept"
	if !strings.Contains(out, want) {
		t.Errorf("output does not contain %q\ngot:\n%s", want, out)
	}
	// Must NOT generate an ip6 rule for an IPv4 address.
	if strings.Contains(out, "ip6 daddr { 192.168.1.10 }") {
		t.Error("should not generate ip6 rule for IPv4 address")
	}
}

func TestGenerateTCPRuleIPv6CIDR(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"2001:db8::/32"},
		Ports: []policy.PortSpec{{Single: 443}},
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := "ip6 daddr { 2001:db8::/32 } tcp dport { 443 } accept"
	if !strings.Contains(out, want) {
		t.Errorf("output does not contain %q\ngot:\n%s", want, out)
	}
	if strings.Contains(out, "ip  daddr { 2001:db8::/32 }") {
		t.Error("should not generate ip4 rule for IPv6 CIDR")
	}
}

func TestGeneratePortRange(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"10.0.0.1"},
		Ports: []policy.PortSpec{{Single: 443}, {IsRange: true, RangeStart: 8080, RangeEnd: 8090}},
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "443, 8080-8090") {
		t.Errorf("expected port set with range, got:\n%s", out)
	}
}

func TestGenerateDropRuleBeforeAcceptRule(t *testing.T) {
	e := &policy.EgressPolicy{
		DefaultAction: "reject",
		Drop:          []policy.Rule{{Proto: "icmp", Type: &policy.ICMPSpec{Name: "echo-request"}}},
		Accept:        []policy.Rule{{Proto: "tcp", To: []string{"10.0.0.1"}, Ports: []policy.PortSpec{{Single: 443}}}},
	}
	out, err := nft.Generate(e)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	dropIdx := strings.Index(out, "drop")
	acceptIdx := strings.Index(out, "accept")
	// The first "accept" will be from implicit invariants; find the user accept rule.
	userAcceptIdx := strings.Index(out, "10.0.0.1")
	if dropIdx > userAcceptIdx {
		t.Error("explicit drop rule must appear before user accept rule")
	}
}

func TestGenerateBlockMetadata(t *testing.T) {
	e := &policy.EgressPolicy{
		DefaultAction: "reject",
		BlockMetadata: true,
	}
	out, err := nft.Generate(e)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, want := range []string{
		"ip  daddr 169.254.169.254/32 drop",
		"ip  daddr 100.100.100.200/32 drop",
		"ip6 daddr fd00:ec2::254/128 drop",
		"ip6 daddr fd20:ce::254/128 drop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("block_metadata: expected %q in output\ngot:\n%s", want, out)
		}
	}
}

func TestGenerateDefaultActionReject(t *testing.T) {
	out, err := nft.Generate(&policy.EgressPolicy{DefaultAction: "reject"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "reject with icmpx type port-unreachable") {
		t.Errorf("expected default reject verdict, got:\n%s", out)
	}
}

func TestGenerateDefaultActionDrop(t *testing.T) {
	out, err := nft.Generate(&policy.EgressPolicy{DefaultAction: "drop"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// The default action line should be just "drop" (not inside a rule).
	if !strings.Contains(out, "\n    drop\n") {
		t.Errorf("expected bare 'drop' verdict line, got:\n%s", out)
	}
}

func TestGenerateDefaultActionAccept(t *testing.T) {
	out, err := nft.Generate(&policy.EgressPolicy{DefaultAction: "accept"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "\n    accept\n") {
		t.Errorf("expected bare 'accept' verdict line, got:\n%s", out)
	}
}

func TestGenerateUnresolvedHostnameComment(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"llm-gateway"},
		Ports: []policy.PortSpec{{Single: 443}},
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "# unresolved: llm-gateway") {
		t.Errorf("expected unresolved comment, got:\n%s", out)
	}
}

// proto with no ports must emit "meta l4proto <proto>" — not be silently dropped.
func TestGenerateProtoTCPNoPortsEmitsL4ProtoMatch(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"10.0.0.1"},
		// Ports deliberately omitted.
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "meta l4proto tcp") {
		t.Errorf("expected 'meta l4proto tcp' for proto-only TCP rule, got:\n%s", out)
	}
	// Must not silently accept all protocols (bare "accept" without proto match).
	if strings.Contains(out, "daddr { 10.0.0.1 } accept") {
		t.Errorf("rule must not drop the protocol restriction:\n%s", out)
	}
}

// proto: udp with no to: and no ports → bare protocol-only rule.
func TestGenerateProtoUDPNoToNoPortsBareRule(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "udp",
		// To and Ports both omitted.
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "meta l4proto udp accept") {
		t.Errorf("expected 'meta l4proto udp accept' for protocol-only rule, got:\n%s", out)
	}
}

// proto+ports path must continue to emit dport set (regression guard).
func TestGenerateProtoAndPortsEmitsDportSet(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"10.0.0.1"},
		Ports: []policy.PortSpec{{Single: 443}, {Single: 80}},
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "tcp dport { 443, 80 } accept") {
		t.Errorf("expected dport set for tcp+ports rule, got:\n%s", out)
	}
	// meta l4proto must NOT appear when dport is already in the rule.
	if strings.Contains(out, "meta l4proto tcp") {
		t.Errorf("dport rule should not also emit meta l4proto:\n%s", out)
	}
}
```

- [ ] **Step 2: Run tests — confirm they fail**

```bash
go test ./pkg/nft/... -run 'TestGenerate' -v 2>&1 | head -20
```

Expected: compile error — `nft.Generate` is undefined.

- [ ] **Step 3: Implement generate.go**

```go
// pkg/nft/generate.go
package nft

import (
	"bytes"
	_ "embed"
	"fmt"
	"net"
	"strings"
	"text/template"

	"vaka.dev/vaka/pkg/policy"
)

//go:embed templates/egress.nft.tmpl
var egressTmpl string

var tmpl = template.Must(template.New("egress").Parse(egressTmpl))

// metadataRanges are the cloud instance metadata endpoints to block.
var metadataRanges = []string{
	"ip  daddr 169.254.169.254/32",
	"ip  daddr 100.100.100.200/32",
	"ip6 daddr fd00:ec2::254/128",
	"ip6 daddr fd20:ce::254/128",
}

// Generate renders the nft ruleset for e.
// If a to: entry is a hostname (not IP/CIDR), it is rendered as a comment
// with a stub — suitable for vaka show output. vaka-init always passes
// pre-resolved policies.
func Generate(e *policy.EgressPolicy) (string, error) {
	data := RulesetData{
		BlockMetadata:  e.BlockMetadata,
		MetadataRanges: metadataRanges,
		DropRules:      expandRules(e.Drop, "drop"),
		RejectRules:    expandRules(e.Reject, "reject"),
		AcceptRules:    expandRules(e.Accept, "accept"),
		DefaultVerdict: defaultVerdict(e.DefaultAction),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render nft template: %w", err)
	}
	return buf.String(), nil
}

// defaultVerdict returns the nft terminal verdict for the default action.
func defaultVerdict(action string) string {
	switch action {
	case "drop":
		return "drop"
	case "accept":
		return "accept"
	default: // "reject" and the defaulted value
		return "reject with icmpx type port-unreachable"
	}
}

// expandRules converts a list of policy Rules into pre-rendered nft rule strings.
func expandRules(rules []policy.Rule, verdict string) []string {
	var out []string
	for _, r := range rules {
		out = append(out, expandRule(r, verdict)...)
	}
	return out
}

// expandRule converts one Rule into one or more nft rule strings.
func expandRule(r policy.Rule, verdict string) []string {
	if r.DNS != nil {
		// DNS rules are expanded by resolve.go before reaching here;
		// a dns: {} that slipped through produces a stub comment.
		return []string{"# dns: {} — not yet resolved"}
	}

	// ICMP rules: generate one rule per applicable family.
	if r.Proto == "icmp" || r.Proto == "icmpv6" {
		return expandICMPRule(r, verdict)
	}

	// Build the combined protocol+port clause.
	// proto=tcp|udp + ports  → "tcp dport { 443 } "
	// proto=tcp|udp, no ports → "meta l4proto tcp " (bare nft protocol matcher)
	// proto empty             → "" (no protocol restriction; validation rejects ports+no-proto)
	protoAndPortClause := ""
	if r.Proto != "" {
		if len(r.Ports) > 0 {
			protoAndPortClause = r.Proto + " dport { " + portList(r.Ports) + " } "
		} else {
			protoAndPortClause = "meta l4proto " + r.Proto + " "
		}
	}

	if len(r.To) == 0 {
		// No destination constraint — bare protocol or verdict-only rule.
		return []string{protoAndPortClause + verdict}
	}

	// Split To entries by IP family.
	var v4, v6, unresolved []string
	for _, addr := range r.To {
		switch classifyAddr(addr) {
		case "ipv4":
			v4 = append(v4, addr)
		case "ipv6":
			v6 = append(v6, addr)
		default:
			unresolved = append(unresolved, addr)
		}
	}

	var lines []string

	if len(unresolved) > 0 {
		for _, name := range unresolved {
			lines = append(lines, fmt.Sprintf("# unresolved: %s", name))
		}
	}

	if len(v4) > 0 {
		line := fmt.Sprintf("ip  daddr { %s } ", strings.Join(v4, ", "))
		line += protoAndPortClause + verdict
		lines = append(lines, line)
	}
	if len(v6) > 0 {
		line := fmt.Sprintf("ip6 daddr { %s } ", strings.Join(v6, ", "))
		line += protoAndPortClause + verdict
		lines = append(lines, line)
	}

	return lines
}

// expandICMPRule handles proto: icmp and proto: icmpv6 rules.
func expandICMPRule(r policy.Rule, verdict string) []string {
	typeClause := ""
	if r.Type != nil {
		typeClause = fmt.Sprintf("icmp type %s ", r.Type.NftString())
		if r.Proto == "icmpv6" {
			typeClause = fmt.Sprintf("icmpv6 type %s ", r.Type.NftString())
		}
	}

	if r.Proto == "icmp" {
		return []string{fmt.Sprintf("meta l4proto icmp  icmp   %s%s", typeClause, verdict)}
	}
	if r.Proto == "icmpv6" {
		return []string{fmt.Sprintf("meta l4proto icmpv6 icmpv6 %s%s", typeClause, verdict)}
	}
	// proto omitted — emit both families.
	return []string{
		fmt.Sprintf("meta l4proto icmp  icmp   %s%s", typeClause, verdict),
		fmt.Sprintf("meta l4proto icmpv6 icmpv6 %s%s", typeClause, verdict),
	}
}

// portList renders a list of PortSpec into a comma-separated nft set string.
func portList(ports []policy.PortSpec) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = p.NftString()
	}
	return strings.Join(parts, ", ")
}

// classifyAddr returns "ipv4", "ipv6", or "hostname".
func classifyAddr(s string) string {
	// Bare IP?
	if ip := net.ParseIP(s); ip != nil {
		if ip.To4() != nil {
			return "ipv4"
		}
		return "ipv6"
	}
	// CIDR?
	if _, ipNet, err := net.ParseCIDR(s); err == nil {
		if ipNet.IP.To4() != nil {
			return "ipv4"
		}
		return "ipv6"
	}
	return "hostname"
}
```

- [ ] **Step 4: Run tests — confirm they pass**

```bash
go test ./pkg/nft/... -run 'TestGenerate' -v
```

Expected: all `TestGenerate*` tests PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/nft/generate.go pkg/nft/generate_test.go pkg/nft/types.go pkg/nft/templates/egress.nft.tmpl
git commit -m "feat(nft): policy-to-ruleset generator with IPv4/IPv6 family split"
```

---

### Task 7: DNS Resolution

**Files:**
- Create: `pkg/nft/resolve.go`
- Create: `pkg/nft/resolve_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// pkg/nft/resolve_test.go
package nft_test

import (
	"strings"
	"testing"

	"vaka.dev/vaka/pkg/nft"
)

func TestParseResolvConf(t *testing.T) {
	input := `
# Generated by NetworkManager
domain example.com
search example.com
nameserver 8.8.8.8
nameserver 2001:4860:4860::8888
nameserver 1.1.1.1
`
	servers, err := nft.ParseResolvConf(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseResolvConf: %v", err)
	}
	if len(servers) != 3 {
		t.Fatalf("len(servers) = %d, want 3", len(servers))
	}
	if servers[0] != "8.8.8.8" {
		t.Errorf("servers[0] = %q, want 8.8.8.8", servers[0])
	}
	if servers[1] != "2001:4860:4860::8888" {
		t.Errorf("servers[1] = %q, want 2001:4860:4860::8888", servers[1])
	}
}

func TestParseResolvConfEmpty(t *testing.T) {
	_, err := nft.ParseResolvConf(strings.NewReader("# no nameservers here\n"))
	if err == nil {
		t.Fatal("expected error for empty resolv.conf, got nil")
	}
}

func TestExpandDNSRuleExplicitServers(t *testing.T) {
	rules, err := nft.ExpandDNS([]string{"8.8.8.8", "1.1.1.1"})
	if err != nil {
		t.Fatalf("ExpandDNS: %v", err)
	}
	// Expect UDP/53 and TCP/53 accept rules for each server.
	combined := strings.Join(rules, "\n")
	if !strings.Contains(combined, "ip  daddr { 8.8.8.8 } udp dport { 53 } accept") {
		t.Errorf("missing UDP/53 rule for 8.8.8.8:\n%s", combined)
	}
	if !strings.Contains(combined, "ip  daddr { 8.8.8.8 } tcp dport { 53 } accept") {
		t.Errorf("missing TCP/53 rule for 8.8.8.8:\n%s", combined)
	}
	if !strings.Contains(combined, "ip  daddr { 1.1.1.1 } udp dport { 53 } accept") {
		t.Errorf("missing UDP/53 rule for 1.1.1.1:\n%s", combined)
	}
}

func TestExpandDNSRuleIPv6Server(t *testing.T) {
	rules, err := nft.ExpandDNS([]string{"2001:4860:4860::8888"})
	if err != nil {
		t.Fatalf("ExpandDNS: %v", err)
	}
	combined := strings.Join(rules, "\n")
	if !strings.Contains(combined, "ip6 daddr { 2001:4860:4860::8888 } udp dport { 53 } accept") {
		t.Errorf("missing ip6 UDP/53 rule:\n%s", combined)
	}
}

// dns: { servers: [...] } must succeed even when resolv.conf is absent/empty.
func TestResolvePolicyExplicitServersDoNotRequireResolvConf(t *testing.T) {
	ctx := context.Background()
	e := &policy.EgressPolicy{
		DefaultAction: "reject",
		Accept: []policy.Rule{
			{DNS: &policy.DNSRule{Servers: []string{"8.8.8.8", "1.1.1.1"}}},
		},
	}
	// Pass nil resolvConf — must succeed because no rule needs resolv.conf.
	resolved, err := nft.ResolvePolicy(ctx, e, nil, &fakeResolver{})
	if err != nil {
		t.Fatalf("unexpected error with explicit dns.servers and nil resolvConf: %v", err)
	}
	if len(resolved.Accept) == 0 {
		t.Error("expected accept rules to be generated from explicit servers")
	}
}

// Mixed rules: one dns:{} (needs resolv.conf) + one dns.servers (does not).
// resolv.conf absent → fail because dns:{} still requires it.
func TestResolvePolicyMixedDNSRulesRequireResolvConf(t *testing.T) {
	ctx := context.Background()
	e := &policy.EgressPolicy{
		DefaultAction: "reject",
		Accept: []policy.Rule{
			{DNS: &policy.DNSRule{}},                             // needs resolv.conf
			{DNS: &policy.DNSRule{Servers: []string{"8.8.8.8"}}}, // does not
		},
	}
	// nil resolvConf → must error because one rule has empty servers.
	_, err := nft.ResolvePolicy(ctx, e, nil, &fakeResolver{})
	if err == nil {
		t.Fatal("expected error when dns:{} is present and resolvConf is nil")
	}
}

// No dns: rules at all → resolvConf is never accessed (can be nil).
func TestResolvePolicyNoDNSRulesSkipsResolvConf(t *testing.T) {
	ctx := context.Background()
	e := &policy.EgressPolicy{
		DefaultAction: "reject",
		Accept: []policy.Rule{
			{Proto: "tcp", To: []string{"10.0.0.1"}, Ports: []policy.PortSpec{{Single: 443}}},
		},
	}
	// nil resolvConf — must succeed because no dns: rules exist.
	_, err := nft.ResolvePolicy(ctx, e, nil, &fakeResolver{})
	if err != nil {
		t.Fatalf("unexpected error when no dns rules and nil resolvConf: %v", err)
	}
}

// fakeResolver is a no-op Resolver for tests that do not exercise DNS lookups.
type fakeResolver struct{}

func (f *fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	return []string{"192.0.2.1"}, nil // always return a stable test IP
}
```

- [ ] **Step 2: Run tests — confirm they fail**

```bash
go test ./pkg/nft/... -run 'TestParse|TestExpand|TestResolvePolicy' -v 2>&1 | head -20
```

Expected: compile error — `nft.ParseResolvConf`, `nft.ExpandDNS`, `nft.ResolvePolicy`, and `fakeResolver` are undefined.

- [ ] **Step 3: Implement resolve.go**

```go
// pkg/nft/resolve.go
package nft

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"

	"vaka.dev/vaka/pkg/policy"
)

// ParseResolvConf extracts nameserver addresses from a resolv.conf reader.
// Returns an error if no nameservers are found.
func ParseResolvConf(r io.Reader) ([]string, error) {
	var servers []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "nameserver") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		servers = append(servers, fields[1])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read resolv.conf: %w", err)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("resolv.conf contains no nameserver entries")
	}
	return servers, nil
}

// ExpandDNS returns pre-rendered nft accept rule strings for UDP/53 and
// TCP/53 to each of the given nameserver addresses.
func ExpandDNS(servers []string) ([]string, error) {
	var rules []string
	for _, srv := range servers {
		family, err := addrFamily(srv)
		if err != nil {
			return nil, fmt.Errorf("invalid nameserver address %q: %w", srv, err)
		}
		for _, proto := range []string{"udp", "tcp"} {
			rules = append(rules,
				fmt.Sprintf("%s daddr { %s } %s dport { 53 } accept", family, srv, proto),
			)
		}
	}
	return rules, nil
}

// addrFamily returns "ip " or "ip6" for IPv4 or IPv6 addresses respectively.
func addrFamily(addr string) (string, error) {
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", fmt.Errorf("not a valid IP address")
	}
	if ip.To4() != nil {
		return "ip ", nil
	}
	return "ip6", nil
}

// Resolver is the interface used for DNS lookups, injectable for testing.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// ResolvePolicy resolves all DNS names and dns: {} entries in e, replacing
// hostnames in rule To: lists with their resolved IPs. Returns a new
// EgressPolicy; the original is not modified.
//
// resolvConf is only read when at least one dns: rule has no explicit servers
// (i.e. NeedsResolvConf returns true). If all dns: rules carry explicit
// servers, resolvConf may safely be nil.
func ResolvePolicy(ctx context.Context, e *policy.EgressPolicy, resolvConf io.Reader, r Resolver) (*policy.EgressPolicy, error) {
	if r == nil {
		r = net.DefaultResolver
	}

	// Read nameservers only for dns: {} rules that don't override with explicit
	// servers. If all dns: rules have explicit servers, resolv.conf is not
	// accessed and resolvConf may be nil.
	var nameservers []string
	if NeedsResolvConf(e) {
		if resolvConf == nil {
			return nil, fmt.Errorf("dns: {} requires /etc/resolv.conf but no reader was provided")
		}
		ns, err := ParseResolvConf(resolvConf)
		if err != nil {
			return nil, fmt.Errorf("dns: {}: %w", err)
		}
		nameservers = ns
	}

	resolved := &policy.EgressPolicy{
		DefaultAction: e.DefaultAction,
		BlockMetadata: e.BlockMetadata,
	}

	var err error
	resolved.Accept, err = resolveRules(ctx, e.Accept, nameservers, r)
	if err != nil {
		return nil, fmt.Errorf("accept rules: %w", err)
	}
	resolved.Reject, err = resolveRules(ctx, e.Reject, nameservers, r)
	if err != nil {
		return nil, fmt.Errorf("reject rules: %w", err)
	}
	resolved.Drop, err = resolveRules(ctx, e.Drop, nameservers, r)
	if err != nil {
		return nil, fmt.Errorf("drop rules: %w", err)
	}

	return resolved, nil
}

// NeedsResolvConf returns true only when at least one dns: rule has no
// explicit servers set, meaning /etc/resolv.conf must be parsed.
// A dns: rule with explicit servers does NOT require resolv.conf.
// Exported so vaka-init can decide whether to open /etc/resolv.conf before
// calling ResolvePolicy.
func NeedsResolvConf(e *policy.EgressPolicy) bool {
	for _, rules := range [][]policy.Rule{e.Accept, e.Reject, e.Drop} {
		for _, r := range rules {
			if r.DNS != nil && len(r.DNS.Servers) == 0 {
				return true
			}
		}
	}
	return false
}

// resolveRules expands dns: {} and resolves hostnames in each rule.
func resolveRules(ctx context.Context, rules []policy.Rule, nameservers []string, r Resolver) ([]policy.Rule, error) {
	var out []policy.Rule
	for _, rule := range rules {
		if rule.DNS != nil {
			// Expand dns: {} into synthetic TCP+UDP accept rules with pre-resolved IPs.
			servers := nameservers
			if len(rule.DNS.Servers) > 0 {
				servers = rule.DNS.Servers
			}
			// dns: {} becomes a set of concrete rules (one per server+proto).
			// We inject them as to: [ip1, ip2...] port: [53] entries for both TCP and UDP.
			dnsRule := policy.Rule{
				To:    servers,
				Ports: []policy.PortSpec{{Single: 53}},
			}
			for _, proto := range []string{"udp", "tcp"} {
				dr := dnsRule
				dr.Proto = proto
				out = append(out, dr)
			}
			continue
		}

		// Resolve hostnames in To:.
		resolved := rule
		resolved.To = nil
		for _, addr := range rule.To {
			if classifyAddr(addr) != "hostname" {
				resolved.To = append(resolved.To, addr)
				continue
			}
			addrs, err := r.LookupHost(ctx, addr)
			if err != nil {
				return nil, fmt.Errorf("resolve %q: %w", addr, err)
			}
			resolved.To = append(resolved.To, addrs...)
		}
		out = append(out, resolved)
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests — confirm they pass**

```bash
go test ./pkg/nft/... -v
```

Expected: all `TestParse*`, `TestExpand*`, and `TestGenerate*` tests PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/nft/resolve.go pkg/nft/resolve_test.go
git commit -m "feat(nft): resolv.conf parser and policy hostname resolution"
```

---

### Task 8: Compose Override Generator

**Files:**
- Create: `pkg/compose/override.go`
- Create: `pkg/compose/override_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// pkg/compose/override_test.go
package compose_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/compose"
)

type overrideDoc struct {
	Secrets  map[string]struct {
		Environment string `yaml:"environment"`
	} `yaml:"secrets"`
	Services map[string]struct {
		Entrypoint []string `yaml:"entrypoint"`
		Command    []string `yaml:"command"`
		CapAdd     []string `yaml:"cap_add"`
		Secrets    []struct {
			Source string `yaml:"source"`
			Target string `yaml:"target"`
		} `yaml:"secrets"`
	} `yaml:"services"`
}

func TestOverrideSecretNameDerivedFromService(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:        "codex",
			Entrypoint:  []string{"claude"},
			Command:     []string{"--dangerously-skip-permissions"},
			CapDelta:    []string{"NET_ADMIN"},
			EnvVarName:  "VAKA_CODEX_CONF",
		},
	}
	out, err := compose.BuildOverride(entries)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}

	var doc overrideDoc
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := doc.Secrets["vaka_codex_conf"]; !ok {
		t.Errorf("expected secret key 'vaka_codex_conf'; got secrets: %+v", doc.Secrets)
	}
	if doc.Secrets["vaka_codex_conf"].Environment != "VAKA_CODEX_CONF" {
		t.Errorf("secret env = %q, want VAKA_CODEX_CONF", doc.Secrets["vaka_codex_conf"].Environment)
	}
}

func TestOverrideEntrypointIsVakaInit(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:       "codex",
			Entrypoint: []string{"claude"},
			Command:    []string{"--dangerously-skip-permissions"},
			EnvVarName: "VAKA_CODEX_CONF",
		},
	}
	out, _ := compose.BuildOverride(entries)

	var doc overrideDoc
	yaml.Unmarshal([]byte(out), &doc) //nolint
	svc := doc.Services["codex"]
	if len(svc.Entrypoint) < 2 || svc.Entrypoint[0] != "vaka-init" || svc.Entrypoint[1] != "--" {
		t.Errorf("entrypoint = %v, want [vaka-init --]", svc.Entrypoint)
	}
}

func TestOverrideCommandIsOriginalEntrypoint(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:       "codex",
			Entrypoint: []string{"claude"},
			Command:    []string{"--dangerously-skip-permissions"},
			EnvVarName: "VAKA_CODEX_CONF",
		},
	}
	out, _ := compose.BuildOverride(entries)

	var doc overrideDoc
	yaml.Unmarshal([]byte(out), &doc) //nolint
	svc := doc.Services["codex"]
	// command must be: original entrypoint + original command concatenated
	if len(svc.Command) == 0 || svc.Command[0] != "claude" {
		t.Errorf("command = %v, want [claude --dangerously-skip-permissions]", svc.Command)
	}
}

func TestOverrideCapAddContainsDelta(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:       "codex",
			Entrypoint: []string{"claude"},
			CapDelta:   []string{"NET_ADMIN"},
			EnvVarName: "VAKA_CODEX_CONF",
		},
	}
	out, _ := compose.BuildOverride(entries)

	var doc overrideDoc
	yaml.Unmarshal([]byte(out), &doc) //nolint
	for _, cap := range doc.Services["codex"].CapAdd {
		if cap == "NET_ADMIN" {
			return
		}
	}
	t.Errorf("cap_add does not contain NET_ADMIN; got %v\n%s", doc.Services["codex"].CapAdd, out)
}

func TestOverrideSecretMountTargetIsVakaYaml(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:       "codex",
			Entrypoint: []string{"claude"},
			EnvVarName: "VAKA_CODEX_CONF",
		},
	}
	out, _ := compose.BuildOverride(entries)

	var doc overrideDoc
	yaml.Unmarshal([]byte(out), &doc) //nolint
	secrets := doc.Services["codex"].Secrets
	if len(secrets) == 0 {
		t.Fatal("no secrets in service override")
	}
	if secrets[0].Target != "vaka.yaml" {
		t.Errorf("secret target = %q, want vaka.yaml", secrets[0].Target)
	}
}

func TestOverrideHyphensInServiceNameBecomesUnderscores(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:       "llm-gateway",
			Entrypoint: []string{"/usr/local/bin/litellm"},
			EnvVarName: "VAKA_LLM_GATEWAY_CONF",
		},
	}
	out, _ := compose.BuildOverride(entries)
	if !strings.Contains(out, "vaka_llm_gateway_conf") {
		t.Errorf("expected secret key with underscores; got:\n%s", out)
	}
}
```

- [ ] **Step 2: Run tests — confirm they fail**

```bash
go test ./pkg/compose/... -run 'TestOverride' -v 2>&1 | head -20
```

Expected: compile error — `compose.BuildOverride` and `compose.ServiceEntry` are undefined.

- [ ] **Step 3: Implement override.go**

```go
// pkg/compose/override.go
package compose

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServiceEntry holds per-service data needed to build the compose override.
type ServiceEntry struct {
	// Name is the docker-compose service name.
	Name string
	// Entrypoint is the harness's original entrypoint (from image or compose).
	Entrypoint []string
	// Command is the harness's original command (from image or compose).
	Command []string
	// CapDelta is the list of capabilities vaka adds that must later be dropped.
	CapDelta []string
	// EnvVarName is the VAKA_<SERVICE>_CONF env var name for the secret.
	EnvVarName string
}

// secretKey returns the compose secret key for a service name.
// "llm-gateway" → "vaka_llm_gateway_conf"
func secretKey(serviceName string) string {
	return "vaka_" + strings.ReplaceAll(strings.ToLower(serviceName), "-", "_") + "_conf"
}

// composeOverride is the typed struct marshaled to YAML.
type composeOverride struct {
	Secrets  map[string]secretDef         `yaml:"secrets,omitempty"`
	Services map[string]serviceOverride   `yaml:"services,omitempty"`
}

type secretDef struct {
	Environment string `yaml:"environment"`
}

type serviceOverride struct {
	Entrypoint []string      `yaml:"entrypoint,omitempty"`
	Command    []string      `yaml:"command,omitempty"`
	CapAdd     []string      `yaml:"cap_add,omitempty"`
	Secrets    []secretMount `yaml:"secrets,omitempty"`
}

type secretMount struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

// BuildOverride constructs the compose override YAML string from entries.
// The result is passed to docker compose via stdin (-f -).
func BuildOverride(entries []ServiceEntry) (string, error) {
	override := composeOverride{
		Secrets:  make(map[string]secretDef),
		Services: make(map[string]serviceOverride),
	}

	for _, e := range entries {
		key := secretKey(e.Name)
		override.Secrets[key] = secretDef{Environment: e.EnvVarName}

		// vaka-init replaces the entrypoint; the original entrypoint+command
		// is passed as arguments after "--".
		cmd := append(e.Entrypoint, e.Command...)

		svc := serviceOverride{
			Entrypoint: []string{"vaka-init", "--"},
			Command:    cmd,
			CapAdd:     e.CapDelta,
			Secrets: []secretMount{{
				Source: key,
				Target: "vaka.yaml",
			}},
		}
		override.Services[e.Name] = svc
	}

	data, err := yaml.Marshal(override)
	if err != nil {
		return "", fmt.Errorf("marshal compose override: %w", err)
	}
	return string(data), nil
}
```

- [ ] **Step 4: Run tests — confirm they pass**

```bash
go test ./pkg/compose/... -v
```

Expected: all `TestOverride*` tests PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/compose/override.go pkg/compose/override_test.go
git commit -m "feat(compose): typed compose override struct with yaml.Marshal output"
```

---

### Task 9: vaka-init Binary

**Files:**
- Create: `cmd/vaka-init/main.go`

This binary runs inside the container. It has no unit tests for the full flow (that requires root + nftables), but each logical step is isolated in a callable function for testability.

- [ ] **Step 1: Write main.go**

```go
// cmd/vaka-init/main.go
//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/syndtr/gocapability/capability"
	"golang.org/x/sys/unix"
	"vaka.dev/vaka/pkg/nft"
	"vaka.dev/vaka/pkg/policy"
)

const secretPath = "/run/secrets/vaka.yaml"
const nftBin = "/opt/vaka/sbin/nft"

func main() {
	if len(os.Args) < 2 || os.Args[1] != "--" {
		fatal("usage: vaka-init -- <entrypoint> [args...]")
	}
	harness := os.Args[2:]
	if len(harness) == 0 {
		fatal("vaka-init: no harness command after --")
	}

	// Step 1: Read and parse per-service policy from Docker secret.
	f, err := os.Open(secretPath)
	if err != nil {
		fatal("open %s: %v", secretPath, err)
	}
	p, err := policy.Parse(f)
	f.Close()
	if err != nil {
		fatal("parse policy: %v", err)
	}
	if len(p.Services) != 1 {
		fatal("policy must contain exactly one service, got %d", len(p.Services))
	}
	if p.APIVersion != "vaka.dev/v1alpha1" {
		fatal("unsupported apiVersion: %s", p.APIVersion)
	}
	var svcName string
	var svc *policy.ServiceConfig
	for k, v := range p.Services {
		svcName, svc = k, v
	}
	if svc.Network == nil || svc.Network.Egress == nil {
		fatal("service %s: no network.egress policy", svcName)
	}
	egress := svc.Network.Egress

	// Step 2: Resolve dynamic values.
	// Open /etc/resolv.conf only when the policy contains a dns: {} rule with
	// no explicit servers. Policies that use dns.servers exclusively work fine
	// without resolv.conf (e.g. scratch containers).
	var resolveConf io.Reader
	if nft.NeedsResolvConf(egress) {
		f, err := os.Open("/etc/resolv.conf")
		if err != nil {
			fatal("open /etc/resolv.conf: %v", err)
		}
		defer f.Close()
		resolveConf = f
	}
	resolved, err := nft.ResolvePolicy(context.Background(), egress, resolveConf, nil)
	if err != nil {
		fatal("resolve policy: %v", err)
	}

	// Step 3: Generate nft ruleset.
	ruleset, err := nft.Generate(resolved)
	if err != nil {
		fatal("generate ruleset: %v", err)
	}

	// Step 4: Apply ruleset atomically via nft -f /dev/stdin.
	nftCmd := exec.Command(nftBin, "-f", "/dev/stdin")
	nftCmd.Stdin = strings.NewReader(ruleset)
	nftCmd.Stdout = os.Stderr // nft writes diagnostics to stdout
	nftCmd.Stderr = os.Stderr
	if err := nftCmd.Run(); err != nil {
		fatal("nft -f /dev/stdin: %v\nruleset:\n%s", err, ruleset)
	}

	// Step 5: Drop capabilities listed in dropCaps.
	if svc.Runtime != nil && len(svc.Runtime.DropCaps) > 0 {
		if err := dropCaps(svc.Runtime.DropCaps); err != nil {
			fatal("drop caps: %v", err)
		}
	}

	// Step 6: Apply runAs (setresgid then setresuid).
	if svc.Runtime != nil && svc.Runtime.RunAs != nil {
		uid := svc.Runtime.RunAs.UID
		gid := svc.Runtime.RunAs.GID
		// GID must be changed before UID.
		if err := unix.Setresgid(gid, gid, gid); err != nil {
			fatal("setresgid(%d): %v", gid, err)
		}
		if err := unix.Setresuid(uid, uid, uid); err != nil {
			fatal("setresuid(%d): %v", uid, err)
		}
		// Kernel clears E+P automatically on 0→nonzero UID transition.
	}

	// Step 7: execve — replace vaka-init with the harness.
	argv0, err := exec.LookPath(harness[0])
	if err != nil {
		fatal("look up %s: %v", harness[0], err)
	}
	if err := syscall.Exec(argv0, harness, os.Environ()); err != nil {
		fatal("execve %s: %v", argv0, err)
	}
}

// dropCaps removes each named capability from all five sets in the correct order.
// Order: Inheritable → Ambient (clear-all) → Bounding → Effective+Permitted.
func dropCaps(caps []string) error {
	capNums, err := parseCaps(caps)
	if err != nil {
		return err
	}

	// a. Drop from Inheritable.
	c, err := capability.NewPid2(0)
	if err != nil {
		return fmt.Errorf("capability.NewPid2: %w", err)
	}
	if err := c.Load(); err != nil {
		return fmt.Errorf("load caps: %w", err)
	}
	for _, cap := range capNums {
		c.Unset(capability.INHERITABLE, cap)
	}
	if err := c.Apply(capability.INHERITABLE); err != nil {
		return fmt.Errorf("apply inheritable: %w", err)
	}

	// b. Clear all Ambient caps.
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl ambient clear: %w", err)
	}

	// c. Drop from Bounding (requires CAP_SETPCAP in E — present in default Docker caps).
	c2, err := capability.NewPid2(0)
	if err != nil {
		return fmt.Errorf("capability.NewPid2: %w", err)
	}
	if err := c2.Load(); err != nil {
		return fmt.Errorf("load caps: %w", err)
	}
	for _, cap := range capNums {
		c2.Unset(capability.BOUNDS, cap)
	}
	if err := c2.Apply(capability.BOUNDS); err != nil {
		return fmt.Errorf("apply bounds (requires CAP_SETPCAP — is cap_drop: ALL set in compose?): %w", err)
	}

	// d. Drop from Effective + Permitted.
	c3, err := capability.NewPid2(0)
	if err != nil {
		return fmt.Errorf("capability.NewPid2: %w", err)
	}
	if err := c3.Load(); err != nil {
		return fmt.Errorf("load caps: %w", err)
	}
	for _, cap := range capNums {
		c3.Unset(capability.EFFECTIVE, cap)
		c3.Unset(capability.PERMITTED, cap)
	}
	if err := c3.Apply(capability.EFFECTIVE | capability.PERMITTED); err != nil {
		return fmt.Errorf("apply effective+permitted: %w", err)
	}

	return nil
}

// parseCaps converts short-form capability names to capability.Cap values.
// Accepts both "NET_ADMIN" and "CAP_NET_ADMIN".
func parseCaps(names []string) ([]capability.Cap, error) {
	var result []capability.Cap
	for _, name := range names {
		normalized := "cap_" + strings.ToLower(strings.TrimPrefix(strings.ToUpper(name), "CAP_"))
		found := false
		for c := capability.Cap(0); c <= capability.CAP_LAST_CAP; c++ {
			if c.String() == normalized {
				result = append(result, c)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown capability: %q", name)
		}
	}
	return result, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "vaka-init: "+format+"\n", args...)
	os.Exit(1)
}
```

- [ ] **Step 2: Verify the binary builds for Linux**

```bash
CGO_ENABLED=0 GOOS=linux go build -o /dev/null ./cmd/vaka-init/
```

Expected: exit 0, no output.

- [ ] **Step 3: Commit**

```bash
git add cmd/vaka-init/main.go
git commit -m "feat(vaka-init): container init binary with nft, cap drop, and execve"
```

---

### Task 10: vaka CLI — Root, validate, and show

**Files:**
- Create: `cmd/vaka/main.go`
- Create: `cmd/vaka/validate.go`
- Create: `cmd/vaka/show.go`

- [ ] **Step 1: Write main.go (root command)**

```go
// cmd/vaka/main.go
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

var rootCmd = &cobra.Command{
	Use:   "vaka",
	Short: "Secure container layer for AI agentic harnesses",
	Long: `vaka enforces nftables egress policy inside Docker containers running
AI agentic harnesses. Run 'vaka up' instead of 'docker compose up'.`,
	SilenceUsage: true,
}

func main() {
	rootCmd.AddCommand(
		newValidateCmd(),
		newShowCmd(),
		&cobra.Command{
			Use:   "version",
			Short: "Print version",
			Run: func(cmd *cobra.Command, args []string) {
				fmt.Println("vaka", version)
			},
		},
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
```

Note: This is a compilable stub for Task 10. The full manual-dispatch `main()` (routing `up`/`run` to the injection path and everything else to pure passthrough) is written in Task 11 Step 6 after `inject.go` and `up.go` exist.

- [ ] **Step 2: Write validate.go**

```go
// cmd/vaka/validate.go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	"github.com/spf13/cobra"
	"vaka.dev/vaka/pkg/policy"
)

func newValidateCmd() *cobra.Command {
	var vakaFile string
	var composeFiles []string

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate vaka.yaml and print per-service summary",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, networkModes, err := loadAndValidate(vakaFile, composeFiles)
			if err != nil {
				return err
			}
			_ = networkModes

			// Print per-service summary.
			for name, svc := range p.Services {
				e := svc.Network.Egress
				accept := 0
				drop := 0
				reject := 0
				if e != nil {
					accept = len(e.Accept)
					drop = len(e.Drop)
					reject = len(e.Reject)
				}
				action := "reject"
				if e != nil {
					action = e.DefaultAction
				}
				fmt.Printf("✓ %-20s — %d accept rule(s), %d drop rule(s), %d reject rule(s), defaultAction: %s\n",
					name, accept, drop, reject, action)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&vakaFile, "file", "f", "vaka.yaml", "Path to vaka.yaml")
	cmd.Flags().StringArrayVar(&composeFiles, "compose", nil, "Path(s) to compose file(s); repeat for multiple (omit to skip compose checks)")
	return cmd
}

// loadAndValidate reads and validates vaka.yaml, then loads the compose
// project (all composeFiles merged via compose-go) to extract network_mode
// per service for the host-network guard.
// composeFiles may be empty — compose checks are skipped in that case.
// Returns the parsed policy and a map of service name → network_mode.
func loadAndValidate(vakaFile string, composeFiles []string) (*policy.ServicePolicy, map[string]string, error) {
	f, err := os.Open(vakaFile)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", vakaFile, err)
	}
	p, err := policy.Parse(f)
	f.Close()
	if err != nil {
		return nil, nil, err
	}

	// Load compose project for network_mode checks (authoritative merge via compose-go).
	// networkModes is nil when no compose files are given — policy.Validate treats nil
	// as "no compose data available, skip compose-dependent checks".
	// When composeFiles is non-empty any loading error is surfaced immediately.
	var networkModes map[string]string
	if len(composeFiles) > 0 {
		opts, err := composecli.NewProjectOptions(composeFiles,
			composecli.WithWorkingDirectory("."),
			composecli.WithOsEnv,
			composecli.WithDotEnv,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("compose project options: %w", err)
		}
		project, err := opts.LoadProject(context.Background())
		if err != nil {
			return nil, nil, fmt.Errorf("load compose project: %w", err)
		}
		networkModes = make(map[string]string)
		for name, svc := range project.Services {
			networkModes[name] = svc.NetworkMode
		}
	}

	errs := policy.Validate(p, networkModes)

	// Warn on defaultAction: accept.
	for name, svc := range p.Services {
		if svc.Network != nil && svc.Network.Egress != nil &&
			svc.Network.Egress.DefaultAction == "accept" {
			fmt.Fprintf(os.Stderr, "WARNING: service %s uses defaultAction: accept — all unmatched egress traffic is allowed.\n", name)
		}
	}

	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = "Error: " + e.Error()
		}
		return nil, nil, fmt.Errorf("%s", strings.Join(msgs, "\n"))
	}

	return p, networkModes, nil
}
```

- [ ] **Step 3: Write show.go**

```go
// cmd/vaka/show.go
package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"vaka.dev/vaka/pkg/nft"
)

func newShowCmd() *cobra.Command {
	var vakaFile string
	var composeFiles []string

	cmd := &cobra.Command{
		Use:   "show <service>",
		Short: "Print the nft ruleset that would be applied for a service (dry-run)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			p, _, err := loadAndValidate(vakaFile, composeFiles)
			if err != nil {
				return err
			}

			svc, ok := p.Services[service]
			if !ok {
				return fmt.Errorf("service %q not found in %s", service, vakaFile)
			}
			if svc.Network == nil || svc.Network.Egress == nil {
				return fmt.Errorf("service %q has no network.egress policy", service)
			}

			// Generate ruleset without DNS resolution.
			// Hostnames in to: appear as unresolved comments.
			out, err := nft.Generate(svc.Network.Egress)
			if err != nil {
				return fmt.Errorf("generate: %w", err)
			}

			fmt.Print(out)
			return nil
		},
	}

	cmd.Flags().StringVarP(&vakaFile, "file", "f", "vaka.yaml", "Path to vaka.yaml")
	cmd.Flags().StringArrayVar(&composeFiles, "compose", nil, "Path(s) to compose file(s); repeat for multiple (omit to skip compose checks)")
	return cmd
}
```

- [ ] **Step 4: Verify the CLI compiles**

```bash
go build -o /dev/null ./cmd/vaka/
```

Expected: exit 0.

- [ ] **Step 5: Smoke-test validate and show with a real vaka.yaml**

Create a temporary test fixture:

```bash
cat > /tmp/test-vaka.yaml <<'EOF'
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  codex:
    network:
      egress:
        defaultAction: reject
        accept:
          - dns: {}
          - proto: tcp
            to: [llm-gateway, 10.20.0.0/16]
            ports: [443]
        block_metadata: true
    runtime:
      runAs:
        uid: 1000
        gid: 1000
EOF
```

Run validate (no --compose flag: compose checks skipped, no error):

```bash
go run ./cmd/vaka/ validate -f /tmp/test-vaka.yaml 2>&1 || true
```

Expected: prints summary line for `codex`.

Run show:

```bash
go run ./cmd/vaka/ show codex -f /tmp/test-vaka.yaml
```

Expected: nft ruleset with `table inet vaka`, implicit invariants, metadata block rules, `# unresolved: llm-gateway`, TCP rule for `10.20.0.0/16`, and `reject with icmpx type port-unreachable`.

- [ ] **Step 6: Commit**

```bash
git add cmd/vaka/main.go cmd/vaka/validate.go cmd/vaka/show.go
git commit -m "feat(vaka): validate and show subcommands"
```

---

### Task 11: vaka up / run Injection and Pure Passthrough

**Files:**
- Create: `cmd/vaka/inject.go`
- Create: `cmd/vaka/inject_test.go`
- Create: `cmd/vaka/up.go`

#### Background: argv flow

`vaka` is a drop-in replacement for `docker compose`. Argument convention is identical:

```
vaka [GLOBAL-FLAGS] SUBCOMMAND [SUBCOMMAND-FLAGS]
```

Docker Compose global flags (`-f`/`--file`, `-p`/`--project-name`, `--profile`, etc.) appear **before** the subcommand. Cobra cannot handle this at the root level — it does not know `-f`, so `vaka -f a.yaml up` would fail with "unknown flag: -f".

**Architecture:** `main()` dispatches argv manually:

1. `extractVakaFlags(raw)` — strips `--vaka-file <val>` from `os.Args[1:]`; returns vaka flags map and cleaned `rest`.
2. `findSubcmd(rest)` — scans `rest` for the first non-flag, non-value token. Uses `composeGlobalFlagsWithValue` to skip value tokens for known compose global flags.
3. Switch on subcommand:
   - `"validate"`, `"show"`, `"version"`, `""` → `rootCmd.SetArgs(rest); rootCmd.Execute()`
   - `"up"`, `"run"` → `runInjection(vakaFile, rest)`
   - anything else → pure passthrough: `exec docker ["compose"] + rest`

**Known compose global flags that take a value** (for `findSubcmd` to skip their values):
`-f`/`--file`, `-p`/`--project-name`, `--profile`, `--env-file`, `--project-directory`, `--parallel`, `--context`/`-c`

**Injection algorithm** (inside `runInjection(vakaFile, args)`):

1. `dockerArgs = ["compose"] + args` (args already contains global flags and subcommand at the correct positions)
2. Scan `dockerArgs[1:]` left to right, stop at `--`:
   - `-f VALUE` or `--file VALUE` (two tokens) → track position of `VALUE` token
   - `--file=VALUE` (one token) → track position of this token
3. If any `-f` found: insert `["-f", "-"]` after the last file value token
4. If none found: `discoverComposeFiles(".")` → if files exist insert `-f <file>… -f -` at index 1; if no files found → return error "no compose configuration file found in current directory"

**Default compose file discovery** (mirrors Docker Compose behaviour):

- Primary: first of `docker-compose.yaml`, `docker-compose.yml` that exists
- Override: first of `docker-compose.override.yaml`, `docker-compose.override.yml` that exists
- Insert all found (primary first, then override) before `-f -`

**Correct examples:**

```
vaka -f a.yaml -f b.yaml up --build
→ docker compose -f a.yaml -f b.yaml -f - up --build

vaka --vaka-file vaka.yaml --file a.yaml -f b.yaml run -ti --rm svc bash
→ docker compose --file a.yaml -f b.yaml -f - run -ti --rm svc bash

vaka up   (docker-compose.yaml exists)
→ docker compose -f ./docker-compose.yaml -f - up

vaka up   (no defaults found)
→ ERROR: no compose configuration file found in current directory

vaka -f dupa.yaml exec svc bash
→ docker compose -f dupa.yaml exec svc bash   (pure passthrough)
```

- [ ] **Step 1: Write inject_test.go (failing)**

```go
// cmd/vaka/inject_test.go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInjectStdinOverride(t *testing.T) {
	t.Run("last -f gets -f - appended after it", func(t *testing.T) {
		args := []string{"compose", "-f", "a.yaml", "-f", "b.yaml", "up", "--build"}
		got := injectStdinOverride(args, nil)
		want := []string{"compose", "-f", "a.yaml", "-f", "b.yaml", "-f", "-", "up", "--build"}
		assertArgv(t, want, got)
	})

	t.Run("--file=value single-token form", func(t *testing.T) {
		args := []string{"compose", "--file=a.yaml", "up"}
		got := injectStdinOverride(args, nil)
		want := []string{"compose", "--file=a.yaml", "-f", "-", "up"}
		assertArgv(t, want, got)
	})

	t.Run("-f before -- is found; -f after -- is ignored", func(t *testing.T) {
		args := []string{"compose", "-f", "a.yaml", "run", "--", "-f", "trick"}
		got := injectStdinOverride(args, nil)
		want := []string{"compose", "-f", "a.yaml", "-f", "-", "run", "--", "-f", "trick"}
		assertArgv(t, want, got)
	})

	t.Run("no -f: inject discovered defaults then -f -", func(t *testing.T) {
		defaults := []string{"docker-compose.yaml", "docker-compose.override.yaml"}
		args := []string{"compose", "up", "--build"}
		got := injectStdinOverride(args, defaults)
		want := []string{
			"compose",
			"-f", "docker-compose.yaml",
			"-f", "docker-compose.override.yaml",
			"-f", "-",
			"up", "--build",
		}
		assertArgv(t, want, got)
	})

	// NOTE: The "no -f and no defaults" case is not tested here.
	// runInjection returns an error before calling injectStdinOverride when
	// no -f flags are present and discoverComposeFiles returns nothing.
}

func TestExtractVakaFlags(t *testing.T) {
	t.Run("extracts --vaka-file and leaves compose global flags in rest", func(t *testing.T) {
		raw := []string{"--vaka-file", "vaka.yaml", "-f", "a.yaml", "up", "--build"}
		flags, rest := extractVakaFlags(raw)
		if flags["--vaka-file"] != "vaka.yaml" {
			t.Fatalf("expected vaka-file=vaka.yaml, got %v", flags)
		}
		want := []string{"-f", "a.yaml", "up", "--build"}
		assertArgv(t, want, rest)
	})

	t.Run("no vaka flags: argv unchanged", func(t *testing.T) {
		raw := []string{"up", "--build"}
		flags, rest := extractVakaFlags(raw)
		if len(flags) != 0 {
			t.Fatalf("expected no flags, got %v", flags)
		}
		assertArgv(t, raw, rest)
	})
}

func TestDiscoverComposeFiles(t *testing.T) {
	dir := t.TempDir()

	t.Run("yaml primary + override.yml", func(t *testing.T) {
		primary := filepath.Join(dir, "docker-compose.yaml")
		override := filepath.Join(dir, "docker-compose.override.yml")
		os.WriteFile(primary, []byte("version: '3'"), 0644)
		os.WriteFile(override, []byte("version: '3'"), 0644)

		got := discoverComposeFiles(dir)
		want := []string{primary, override}
		assertArgv(t, want, got)

		os.Remove(primary)
		os.Remove(override)
	})

	t.Run("neither exists: empty", func(t *testing.T) {
		got := discoverComposeFiles(dir)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})
}

func TestAllFileFlags(t *testing.T) {
	t.Run("multiple -f forms collected in order", func(t *testing.T) {
		args := []string{"-f", "a.yaml", "--file", "b.yaml", "--file=c.yaml", "up"}
		got := allFileFlags(args)
		want := []string{"a.yaml", "b.yaml", "c.yaml"}
		assertArgv(t, want, got)
	})

	t.Run("stops at --", func(t *testing.T) {
		args := []string{"-f", "a.yaml", "run", "--", "-f", "trick.yaml"}
		got := allFileFlags(args)
		want := []string{"a.yaml"}
		assertArgv(t, want, got)
	})

	t.Run("stops at subcommand: -f after run is not a compose file", func(t *testing.T) {
		// -f output.txt is an arg to the command being run, not a compose file.
		args := []string{"run", "--rm", "svc", "myapp", "-f", "output.txt"}
		got := allFileFlags(args)
		if len(got) != 0 {
			t.Fatalf("expected empty (stopped at subcommand), got %v", got)
		}
	})

	t.Run("unknown value-taking global flag does not swallow subcommand", func(t *testing.T) {
		// --ansi is a value-taking flag; its value must not be mistaken for subcommand.
		args := []string{"--ansi", "always", "-f", "a.yaml", "up"}
		got := allFileFlags(args)
		want := []string{"a.yaml"}
		assertArgv(t, want, got)
	})

	t.Run("no -f flags: empty result", func(t *testing.T) {
		args := []string{"up", "--build"}
		got := allFileFlags(args)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})
}

func TestFindSubcmd(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"up", "--build"}, "up"},
		{[]string{"-f", "a.yaml", "up", "--build"}, "up"},
		{[]string{"--file", "a.yaml", "-f", "b.yaml", "run", "--rm", "svc"}, "run"},
		{[]string{"--profile", "myprofile", "run", "--rm", "svc"}, "run"},
		{[]string{"--file=a.yaml", "exec", "svc", "bash"}, "exec"},
		{[]string{"-p", "myproject", "ps"}, "ps"},
		// --ansi and --progress take a value; their value must not be mistaken for subcommand.
		{[]string{"--ansi", "always", "up", "--build"}, "up"},
		{[]string{"--progress", "plain", "-f", "a.yaml", "run"}, "run"},
		{[]string{}, ""},
	}
	for _, tc := range tests {
		got := findSubcmd(tc.args)
		if got != tc.want {
			t.Errorf("findSubcmd(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func assertArgv(t *testing.T, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("length mismatch\nwant %v\n got  %v", want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("index %d: want %q got %q\nfull want: %v\nfull got:  %v", i, want[i], got[i], want, got)
		}
	}
}
```

- [ ] **Step 2: Run the test to confirm it fails**

```bash
cd /home/emsi/git/vaka && go test ./cmd/vaka/ -run 'TestInjectStdinOverride|TestExtractVakaFlags|TestDiscoverComposeFiles|TestFindSubcmd|TestAllFileFlags' -v 2>&1 | head -20
```

Expected: compilation error — `injectStdinOverride`, `extractVakaFlags`, `discoverComposeFiles`, `findSubcmd`, `allFileFlags` not defined.

- [ ] **Step 3: Write inject.go**

```go
// cmd/vaka/inject.go
package main

import (
	"os"
	"path/filepath"
	"strings"
)

// vakaFlagsTakingValue lists --vaka-* flags that consume the next token as their value.
var vakaFlagsTakingValue = map[string]bool{
	"--vaka-file": true,
}

// extractVakaFlags splits raw os.Args[1:] into vaka-specific flags (returned as
// a map of flag→value) and the remaining compose-destined args.
// Only flags in vakaFlagsTakingValue are recognised; unknown --vaka-* flags are
// left in rest so docker compose can reject them with a clear error.
func extractVakaFlags(argv []string) (flags map[string]string, rest []string) {
	flags = make(map[string]string)
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if vakaFlagsTakingValue[arg] {
			if i+1 < len(argv) {
				flags[arg] = argv[i+1]
				i++ // consume value token
			}
			continue
		}
		rest = append(rest, arg)
	}
	return flags, rest
}

// discoverComposeFiles returns the default compose files that Docker Compose
// would load from dir when no explicit -f flags are given, in the order they
// would be merged (primary first, then override).
func discoverComposeFiles(dir string) []string {
	var found []string

	primaries := []string{"docker-compose.yaml", "docker-compose.yml"}
	for _, name := range primaries {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
			break
		}
	}

	overrides := []string{"docker-compose.override.yaml", "docker-compose.override.yml"}
	for _, name := range overrides {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
			break
		}
	}

	return found
}

// injectStdinOverride takes dockerArgs (already prefixed with "compose") and
// inserts "-f -" as the last -f argument so the vaka override YAML (piped on
// stdin) wins over all other compose files.
//
// defaults is the list of compose files to inject when the user supplied no
// explicit -f flags (output of discoverComposeFiles). Pass nil only when -f
// flags are already present in dockerArgs. Callers must NOT pass nil defaults
// when there are also no -f flags in dockerArgs — that case must be caught by
// the caller and returned as an error before calling injectStdinOverride.
func injectStdinOverride(dockerArgs []string, defaults []string) []string {
	// Find the position of the value token belonging to the last -f/--file
	// before any -- end-of-options marker.
	lastFileValueIdx := -1
	for i := 1; i < len(dockerArgs); i++ {
		tok := dockerArgs[i]
		if tok == "--" {
			break
		}
		if tok == "-f" || tok == "--file" {
			if i+1 < len(dockerArgs) {
				lastFileValueIdx = i + 1
				i++ // skip value token
			}
		} else if strings.HasPrefix(tok, "--file=") {
			lastFileValueIdx = i
		}
	}

	out := make([]string, 0, len(dockerArgs)+len(defaults)*2+2)

	if lastFileValueIdx >= 0 {
		// Insert "-f", "-" immediately after the last file value token.
		out = append(out, dockerArgs[:lastFileValueIdx+1]...)
		out = append(out, "-f", "-")
		out = append(out, dockerArgs[lastFileValueIdx+1:]...)
	} else {
		// No explicit -f: insert discovered defaults then "-f", "-" at index 1
		// (right after "compose", before any subcommand or other flags).
		out = append(out, dockerArgs[0]) // "compose"
		for _, f := range defaults {
			out = append(out, "-f", f)
		}
		out = append(out, "-f", "-")
		out = append(out, dockerArgs[1:]...)
	}

	return out
}

// composeGlobalFlagsWithValue is the set of docker compose global flags that
// consume the next token as their value. Both allFileFlags and findSubcmd use
// this to skip value tokens when scanning for the subcommand boundary.
//
// Keep in sync with: docker compose --help (global options section).
// Flags known to take a value as of Docker Compose v2:
//   -f/--file, -p/--project-name, --profile, --env-file,
//   --project-directory, --parallel, --context/-c, --ansi, --progress
var composeGlobalFlagsWithValue = map[string]bool{
	"-f": true, "--file": true,
	"-p": true, "--project-name": true,
	"--profile":           true,
	"--env-file":          true,
	"--project-directory": true,
	"--parallel":          true,
	"--context":           true,
	"-c":                  true,
	"--ansi":              true,
	"--progress":          true,
}

// allFileFlags returns all -f / --file values from args that appear before
// the subcommand boundary, in order. Scanning stops at -- or at the first
// bare-word token (the subcommand); compose global flags only appear before
// the subcommand, so any -f after it belongs to the subcommand or downstream
// command (e.g. the command run by "docker compose run").
func allFileFlags(args []string) []string {
	var files []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			break
		}
		if tok == "-f" || tok == "--file" {
			if i+1 < len(args) {
				files = append(files, args[i+1])
				i++
			}
			continue
		}
		if strings.HasPrefix(tok, "--file=") {
			files = append(files, strings.TrimPrefix(tok, "--file="))
			continue
		}
		// Other global flags that take a value: skip their value token.
		if composeGlobalFlagsWithValue[tok] {
			i++
			continue
		}
		// --flag=value: no separate value token.
		if strings.HasPrefix(tok, "--") && strings.Contains(tok, "=") {
			continue
		}
		// Boolean flag.
		if strings.HasPrefix(tok, "-") {
			continue
		}
		// First bare-word token is the subcommand: stop here.
		break
	}
	return files
}

// findSubcmd returns the first non-flag, non-value token from args (the compose
// subcommand). Returns "" if no subcommand is found.
func findSubcmd(args []string) string {
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			break
		}
		if composeGlobalFlagsWithValue[tok] {
			i++ // skip value token
			continue
		}
		if strings.HasPrefix(tok, "--") && strings.Contains(tok, "=") {
			continue // --flag=value: no separate value token
		}
		if strings.HasPrefix(tok, "-") {
			continue // boolean flag
		}
		return tok
	}
	return ""
}
```

- [ ] **Step 4: Run the injection tests**

```bash
cd /home/emsi/git/vaka && go test ./cmd/vaka/ -run 'TestInjectStdinOverride|TestExtractVakaFlags|TestDiscoverComposeFiles|TestFindSubcmd|TestAllFileFlags' -v
```

Expected: all tests PASS.

- [ ] **Step 5: Write up.go**

```go
// cmd/vaka/up.go
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/compose"
	"vaka.dev/vaka/pkg/policy"
)

// defaultDockerCaps is the set of capabilities present in a default Docker
// container (no cap_drop, no cap_add). NET_ADMIN is notably absent.
var defaultDockerCaps = map[string]bool{
	"CAP_CHOWN": true, "CAP_DAC_OVERRIDE": true, "CAP_FOWNER": true,
	"CAP_FSETID": true, "CAP_KILL": true, "CAP_SETGID": true,
	"CAP_SETUID": true, "CAP_SETPCAP": true, "CAP_NET_BIND_SERVICE": true,
	"CAP_NET_RAW": true, "CAP_SYS_CHROOT": true, "CAP_MKNOD": true,
	"CAP_AUDIT_WRITE": true, "CAP_SETFCAP": true,
}

func newUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "up [compose-flags...]",
		Short:              "Validate, inject vaka policy, and proxy docker compose up",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			vakaFlags, rest := extractVakaFlags(args)
			vakaFile := vakaFlags["--vaka-file"]
			if vakaFile == "" {
				vakaFile = "vaka.yaml"
			}
			return runInjection(vakaFile, rest)
		},
	}
}

func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "run [compose-flags...]",
		Short:              "Validate, inject vaka policy, and proxy docker compose run",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			vakaFlags, rest := extractVakaFlags(args)
			vakaFile := vakaFlags["--vaka-file"]
			if vakaFile == "" {
				vakaFile = "vaka.yaml"
			}
			return runInjection(vakaFile, rest)
		},
	}
}

// newPassthroughCmd creates a cobra command that forwards everything verbatim
// to "docker compose". Used for exec, build, attach, ps, logs, etc.
func newPassthroughCmd(subcmd string) *cobra.Command {
	return &cobra.Command{
		Use:                subcmd,
		Short:              fmt.Sprintf("Proxy docker compose %s", subcmd),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dockerArgs := append([]string{"compose", subcmd}, args...)
			c := exec.Command("docker", dockerArgs...)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return c.Run()
		},
	}
}

// runInjection is the injection path for "up" and "run":
// 1. Collect all -f files from args (or discover defaults if none).
// 2. Validate vaka.yaml against the merged compose project.
// 3. Load the fully-merged compose project via compose-go (authoritative for
//    entrypoint/cap data — handles multi-file merge, env interpolation, etc.).
// 4. Per service: resolve entrypoint, compute cap delta, serialise policy.
// 5. Build override YAML, inject -f - into argv, exec docker.
func runInjection(vakaFile string, args []string) error {
	composeFiles := allFileFlags(args)
	var defaults []string
	if len(composeFiles) == 0 {
		defaults = discoverComposeFiles(".")
		if len(defaults) == 0 {
			return fmt.Errorf("no compose configuration file found in current directory")
		}
		composeFiles = defaults
	}

	p, _, err := loadAndValidate(vakaFile, composeFiles)
	if err != nil {
		return err
	}

	ctx := context.Background()
	opts, err := composecli.NewProjectOptions(composeFiles,
		composecli.WithWorkingDirectory("."),
		composecli.WithOsEnv,
		composecli.WithDotEnv,
	)
	if err != nil {
		return fmt.Errorf("compose project options: %w", err)
	}
	project, err := opts.LoadProject(ctx)
	if err != nil {
		return fmt.Errorf("load compose project: %w", err)
	}

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer dockerClient.Close()

	var entries []compose.ServiceEntry
	envVars := os.Environ()

	for svcName, svc := range p.Services {
		composeSvc, ok := project.Services[svcName]
		if !ok {
			return fmt.Errorf("service %q: not found in compose files %v", svcName, composeFiles)
		}

		entrypoint, cmd, err := resolveEntrypoint(ctx, dockerClient, svcName, composeSvc)
		if err != nil {
			return err
		}

		delta := computeCapDelta(composeSvc)
		if svc.Runtime == nil {
			svc.Runtime = &policy.RuntimeConfig{}
		}
		if len(svc.Runtime.DropCaps) == 0 {
			svc.Runtime.DropCaps = delta
		}
		fmt.Fprintf(os.Stderr, "vaka: service %s: dropCaps: %v\n", svcName, svc.Runtime.DropCaps)

		sliced := policy.SliceService(p, svcName)
		raw, err := yaml.Marshal(sliced)
		if err != nil {
			return fmt.Errorf("marshal policy for %s: %w", svcName, err)
		}

		envKey := "VAKA_" + strings.ToUpper(strings.ReplaceAll(svcName, "-", "_")) + "_CONF"
		envVars = append(envVars, envKey+"="+base64.StdEncoding.EncodeToString(raw))

		entries = append(entries, compose.ServiceEntry{
			Name:       svcName,
			Entrypoint: entrypoint,
			Command:    cmd,
			CapDelta:   delta,
			EnvVarName: envKey,
		})
	}

	overrideYAML, err := compose.BuildOverride(entries)
	if err != nil {
		return fmt.Errorf("build override: %w", err)
	}

	// args already contains global flags + subcommand at correct positions.
	// Prepend "compose"; injectStdinOverride inserts -f - after the last -f.
	dockerArgs := injectStdinOverride(append([]string{"compose"}, args...), defaults)

	c := exec.Command("docker", dockerArgs...)
	c.Stdin = strings.NewReader(overrideYAML)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = envVars
	return c.Run()
}

// resolveEntrypoint returns the effective entrypoint and command for a service,
// using the already-merged compose-go ServiceConfig (all -f files merged).
// Falls back to Docker SDK image inspection only when neither entrypoint nor
// command is declared in any of the compose files.
func resolveEntrypoint(ctx context.Context, dockerClient *client.Client, svcName string, svc composetypes.ServiceConfig) ([]string, []string, error) {
	if len(svc.Entrypoint) > 0 || len(svc.Command) > 0 {
		return svc.Entrypoint, svc.Command, nil
	}
	if svc.Image == "" {
		return nil, nil, fmt.Errorf("service %s: no image and no entrypoint/command declared", svcName)
	}
	inspect, err := dockerClient.ImageInspect(ctx, svc.Image)
	if err != nil {
		if client.IsErrNotFound(err) {
			return nil, nil, fmt.Errorf(
				"service %s: image %q not available locally and no entrypoint/command declared — pull first or add entrypoint:",
				svcName, svc.Image)
		}
		return nil, nil, fmt.Errorf("service %s: inspect %q: %w", svcName, svc.Image, err)
	}
	if inspect.Config == nil {
		return nil, nil, fmt.Errorf("service %s: image %q has no Config", svcName, svc.Image)
	}
	return inspect.Config.Entrypoint, inspect.Config.Cmd, nil
}

// computeCapDelta returns the capabilities vaka needs that are absent from
// Docker's default set and not already in the merged compose service's cap_add.
func computeCapDelta(svc composetypes.ServiceConfig) []string {
	existing := map[string]bool{}
	for _, cap := range svc.CapAdd {
		existing[strings.ToUpper(cap)] = true
	}
	var delta []string
	for _, cap := range []string{"NET_ADMIN"} {
		if !existing[cap] && !defaultDockerCaps["CAP_"+cap] {
			delta = append(delta, cap)
		}
	}
	return delta
}
```

- [ ] **Step 6: Replace main.go with manual dispatch (definitive version)**

Replace `cmd/vaka/main.go` entirely. This supersedes the Task 10 stub and wires up the manual argv dispatcher:

```go
// cmd/vaka/main.go
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var version = "dev"

var rootCmd = &cobra.Command{
	Use:   "vaka",
	Short: "Secure container layer for AI agentic harnesses",
	Long: `vaka enforces nftables egress policy inside Docker containers running
AI agentic harnesses. Run 'vaka up' instead of 'docker compose up'.`,
	SilenceUsage: true,
}

func main() {
	rootCmd.AddCommand(
		newValidateCmd(),
		newShowCmd(),
		// up and run stubs exist only for --help visibility.
		// Actual execution is handled by the manual dispatch switch below
		// and never reaches these cobra commands.
		&cobra.Command{
			Use:                "up [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose up",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "run [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose run",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:   "version",
			Short: "Print version",
			Run: func(cmd *cobra.Command, args []string) {
				fmt.Println("vaka", version)
			},
		},
	)

	raw := os.Args[1:]

	// Step 1: Extract vaka-specific flags (--vaka-file).
	vakaFlags, rest := extractVakaFlags(raw)
	vakaFile := vakaFlags["--vaka-file"]
	if vakaFile == "" {
		vakaFile = "vaka.yaml"
	}

	// Step 2: Find the subcommand (first non-flag, non-value token).
	subcmd := findSubcmd(rest)

	// Step 3: Route.
	switch subcmd {
	case "validate", "show", "version", "":
		// cobra-handled commands. SetArgs so cobra sees a clean argv
		// (--vaka-file already stripped by extractVakaFlags).
		rootCmd.SetArgs(rest)
		if err := rootCmd.Execute(); err != nil {
			os.Exit(1)
		}

	case "up", "run":
		// Injection path: validate vaka policy and inject -f - into compose argv.
		if err := runInjection(vakaFile, rest); err != nil {
			fmt.Fprintln(os.Stderr, "vaka:", err)
			os.Exit(exitCode(err))
		}

	default:
		// Pure passthrough: prepend "compose" and exec docker verbatim.
		c := exec.Command("docker", append([]string{"compose"}, rest...)...)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			os.Exit(exitCode(err))
		}
	}
}

// exitCode extracts the process exit code from an *exec.ExitError so that
// vaka propagates docker's exit code faithfully.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
```

- [ ] **Step 7: Verify the CLI compiles**

```bash
cd /home/emsi/git/vaka && go build -o /dev/null ./cmd/vaka/
```

Expected: exit 0.

- [ ] **Step 8: Smoke-test --help**

```bash
cd /home/emsi/git/vaka && go run ./cmd/vaka/ --help
```

Expected: usage output listing `up`, `run`, `validate`, `show`, `version`. `exec`, `build`, and other passthrough subcommands do **not** appear — they are handled by the `default:` branch of the manual dispatch switch, not cobra. The `up` and `run` stubs are registered with cobra purely for help visibility; actual execution goes through the dispatch switch.

- [ ] **Step 9: Commit**

```bash
git add cmd/vaka/inject.go cmd/vaka/inject_test.go cmd/vaka/up.go cmd/vaka/main.go
git commit -m "feat(vaka): manual argv dispatch; injection for up/run; pure passthrough for all other subcommands"
```

---

### Task 12: emsi/vaka-init Docker Image

**Files:**
- Create: `docker/init/Dockerfile`

- [ ] **Step 1: Write the Dockerfile**

```dockerfile
# docker/init/Dockerfile
# Builds emsi/vaka-init — scratch image containing:
#   /opt/vaka/bin/nft        (from emsi/nft-static)
#   /opt/vaka/bin/vaka-init  (built from this repo)
#
# Usage in a harness Dockerfile:
#   FROM emsi/vaka-init:latest AS vaka
#   FROM ubuntu:24.04
#   COPY --from=vaka /opt/vaka/sbin/vaka-init /opt/vaka/sbin/vaka-init
#   COPY --from=vaka /opt/vaka/sbin/nft       /opt/vaka/sbin/nft

ARG NFTABLES_IMAGE=emsi/nft-static:1.1.6

FROM ${NFTABLES_IMAGE} AS nft

FROM golang:1.23-alpine AS builder

WORKDIR /src

# Copy go.mod and go.sum first for layer caching.
COPY go.mod go.sum go.work* ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/vaka-init \
    ./cmd/vaka-init

FROM scratch

COPY --from=nft     /opt/nftables/bin/nft   /opt/vaka/bin/nft
COPY --from=builder /out/vaka-init          /opt/vaka/bin/vaka-init
```

- [ ] **Step 2: Build the image locally**

```bash
docker build -f docker/init/Dockerfile -t emsi/vaka-init:dev .
```

Expected: build succeeds; final image is `scratch`-based with two binaries.

- [ ] **Step 3: Verify the image contains only the two expected files**

```bash
docker run --rm emsi/vaka-init:dev ls /opt/vaka/bin/ 2>&1 || \
  docker create --name vaka-init-check emsi/vaka-init:dev && \
  docker export vaka-init-check | tar -t | grep opt/vaka && \
  docker rm vaka-init-check
```

Expected: `nft` and `vaka-init` listed under `opt/vaka/bin/`.

- [ ] **Step 4: Verify vaka-init is statically linked**

```bash
docker run --rm --entrypoint /opt/vaka/bin/vaka-init emsi/vaka-init:dev 2>&1 || true
```

Expected: `vaka-init: usage: vaka-init -- <entrypoint> [args...]` (or the binary runs without linker errors — which would only appear if it had dynamic deps on a scratch image).

- [ ] **Step 5: Commit**

```bash
git add docker/init/Dockerfile
git commit -m "feat(docker): emsi/vaka-init multi-stage Dockerfile with scratch final stage"
```

---

## Self-Review

### Spec coverage check

| Spec requirement | Covered in task |
|---|---|
| `vaka-init` reads `/run/secrets/vaka.yaml` strictly | Task 9 |
| Atomic nft apply via `nft -f /dev/stdin` | Task 9 |
| Fail-closed on any init error | Task 9 (fatal exits) |
| Capability drop order: I → A → B → E/P | Task 9 (`dropCaps`) |
| `setresgid` before `setresuid` | Task 9 |
| Kernel UID fixup clears E+P | Task 9 (documented) |
| `execve` harness | Task 9 |
| `dns: {}` resolv.conf expansion | Task 7 |
| `dns.servers` overrides resolv.conf (resolv.conf not required) | Task 7 (`NeedsResolvConf`, exported); Task 9 (conditional open) |
| `block_metadata` metadata endpoint rules | Tasks 5, 6 |
| IPv4+IPv6 via `inet` table | Tasks 5, 6 |
| `to:` hostname resolution at init time | Task 7 |
| `defaultAction` defaults to `reject` | Task 3 |
| Unknown YAML fields → hard error | Task 3 |
| All §6.3 validation rules | Task 4 |
| Service name is valid DNS label | Task 4 |
| Service name must exist in `docker-compose.yaml` | Task 4 |
| `network_mode: host` hard error | Task 4 |
| ports without proto → validation error | Task 4 |
| `dropCaps` validated against known Linux cap list; `CAP_` prefix stripped | Task 4 |
| proto-only rule emits `meta l4proto` (no silent protocol drop) | Task 6 |
| `defaultAction: accept` warning | Task 10 |
| `vaka validate` per-service summary | Task 10 |
| `vaka show` unresolved hostname comments | Tasks 6, 10 |
| `vaka up` / `vaka run` Docker SDK entrypoint lookup | Task 11 |
| `vaka up` / `vaka run` image-not-local error | Task 11 |
| Delta-based `dropCaps` auto-computed | Task 11 |
| Explicit `dropCaps` in vaka.yaml overrides delta | Task 11 |
| Override piped via `docker compose -f -` (last `-f` wins) | Task 11 |
| `VAKA_*_CONF` env vars on child process | Task 11 |
| `-f -` injection: last existing `-f` wins; no user `-f` → discover defaults | Task 11 |
| `--vaka-file` extracted before passthrough; `--vaka-*` unknown flags left for compose | Task 11 |
| Pure 1:1 passthrough for exec/build/ps/logs/down/attach/pull/push/restart/stop/start | Task 11 |
| Multi-file compose merge correctness via `compose-go/v2` (not hand-rolled) | Tasks 1, 10, 11 |
| `allFileFlags` collects all `-f` values; compose-go merges them all for entrypoint/cap data | Task 11 |
| `emsi/vaka-init` scratch image | Task 12 |
| Harness `ENTRYPOINT` not changed in image | Documented in spec §5.1 |
| Edge case: `cap_drop: ALL` exits with clear error | Covered by the `apply bounds` error message in Task 9 |

### Placeholder scan

No TBD, TODO, or incomplete sections found.

### Type consistency check

- `policy.EgressPolicy`, `policy.Rule`, `policy.PortSpec`, `policy.ICMPSpec`, `policy.DNSRule` — defined in Task 2, used consistently in Tasks 3, 4, 6, 7, 8, 9, 11.
- `nft.RulesetData` — defined in Task 5, populated in Task 6.
- `nft.Generate(e *policy.EgressPolicy)` — defined in Task 6, called in Tasks 9 and 10.
- `nft.ResolvePolicy(ctx, e, reader, resolver)` — defined in Task 7, called in Task 9.
- `compose.ServiceEntry`, `compose.BuildOverride` — defined in Task 8, called in Task 11.
- `policy.SliceService(p, name)` — defined in Task 3, called in Task 11.
- `loadAndValidate(vakaFile, composeFile)` — defined in Task 10, called in Tasks 10 and 11.
- `composetypes.ServiceConfig.Entrypoint` / `.Command` are `[]string` (compose-go `ShellCommand`); `composetypes.ServiceConfig.CapAdd` is `[]string` — no custom YAML decoding needed.
