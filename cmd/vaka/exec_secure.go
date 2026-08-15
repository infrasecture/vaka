package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"vaka.dev/vaka/pkg/compose"
)

const (
	composeContainerNumberLabel = "com.docker.compose.container-number"
	composeConfigHashLabel      = "com.docker.compose.config-hash"
	composeOneoffLabel          = "com.docker.compose.oneoff"
	vakaInitPath                = "/opt/vaka/sbin/vaka-init"
)

type execTarget struct {
	Managed        bool
	RuntimeVersion string
	RuntimeImage   string
	RuntimeMounted bool
}

type execTargetInspector interface {
	InspectExecTarget(context.Context, string, string, int) (execTarget, error)
}

// InspectExecTarget resolves the same service replica selected by Compose
// exec and reads security metadata from the live container. Live labels are
// authoritative: local policy or Compose files may have changed since the
// container was created.
func (d *dockerServices) InspectExecTarget(ctx context.Context, project, service string, index int) (execTarget, error) {
	if d.legacy == nil {
		return execTarget{}, fmt.Errorf("inspect exec target: Docker client is unavailable")
	}
	filterArgs := []filters.KeyValuePair{
		filters.Arg("label", composeProjectLabel+"="+project),
		filters.Arg("label", composeServiceLabel+"="+service),
		filters.Arg("label", composeConfigHashLabel),
	}
	if index > 0 {
		filterArgs = append(filterArgs, filters.Arg("label", composeContainerNumberLabel+"="+strconv.Itoa(index)))
	}
	containers, err := d.legacy.ContainerList(ctx, containertypes.ListOptions{Filters: filters.NewArgs(filterArgs...)})
	if err != nil {
		return execTarget{}, fmt.Errorf("find running container for service %s in project %s on %s: %w", service, project, d.targetDesc, err)
	}
	sort.SliceStable(containers, func(i, j int) bool {
		leftOneoff, rightOneoff := isComposeOneoff(containers[i]), isComposeOneoff(containers[j])
		if leftOneoff != rightOneoff {
			return !leftOneoff
		}
		left, right := containerNumber(containers[i]), containerNumber(containers[j])
		if left != right {
			return left < right
		}
		return false
	})
	for _, ctr := range containers {
		if ctr.Labels[composeProjectLabel] != project || ctr.Labels[composeServiceLabel] != service {
			continue
		}
		if index > 0 && containerNumber(ctr) != index {
			continue
		}
		return execTarget{
			Managed:        ctr.Labels[compose.ManagedLabel] == "true",
			RuntimeVersion: strings.TrimSpace(ctr.Labels[compose.RuntimeVersionLabel]),
			RuntimeImage:   strings.TrimSpace(ctr.Labels[compose.RuntimeImageLabel]),
			RuntimeMounted: verifiedRuntimeMount(ctr),
		}, nil
	}
	if index > 0 {
		return execTarget{}, fmt.Errorf("service %s has no running container at index %d in Compose project %s", service, index, project)
	}
	return execTarget{}, fmt.Errorf("service %s has no running container in Compose project %s", service, project)
}

func verifiedRuntimeMount(ctr containertypes.Summary) bool {
	for _, mounted := range ctr.Mounts {
		if string(mounted.Type) == "image" && mounted.Destination == "/opt/vaka" && !mounted.RW {
			return true
		}
	}
	return false
}

type projectExecutionInspector interface {
	InspectManagedProject(context.Context, string) (map[string][]execTarget, error)
}

func (d *dockerServices) InspectManagedProject(ctx context.Context, project string) (map[string][]execTarget, error) {
	if d.legacy == nil {
		return nil, fmt.Errorf("inspect Compose project: Docker client is unavailable")
	}
	containers, err := d.legacy.ContainerList(ctx, containertypes.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", composeProjectLabel+"="+project),
			filters.Arg("label", composeConfigHashLabel),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("find containers for Compose project %s on %s: %w", project, d.targetDesc, err)
	}
	targets := make(map[string][]execTarget)
	for _, ctr := range containers {
		// Compose lifecycle verbs operate on regular service containers, not
		// one-offs left by `compose run`; stale one-offs must not block start or
		// restart of the declared service.
		if ctr.Labels[compose.ManagedLabel] != "true" || isComposeOneoff(ctr) {
			continue
		}
		service := ctr.Labels[composeServiceLabel]
		if service == "" {
			continue
		}
		targets[service] = append(targets[service], execTarget{
			Managed:        true,
			RuntimeVersion: strings.TrimSpace(ctr.Labels[compose.RuntimeVersionLabel]),
			RuntimeImage:   strings.TrimSpace(ctr.Labels[compose.RuntimeImageLabel]),
			RuntimeMounted: verifiedRuntimeMount(ctr),
		})
	}
	return targets, nil
}

func containerNumber(ctr containertypes.Summary) int {
	n, err := strconv.Atoi(ctr.Labels[composeContainerNumberLabel])
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func isComposeOneoff(ctr containertypes.Summary) bool {
	return strings.EqualFold(ctr.Labels[composeOneoffLabel], "true")
}

type parsedExec struct {
	service    string
	command    []string
	index      int
	prefix     []string
	user       string
	privileged bool
}

var execOptionsWithValue = map[string]bool{
	"-e": true, "--env": true,
	"--index": true,
	"-u":      true, "--user": true,
	"-w": true, "--workdir": true,
}

var execBooleanOptions = map[string]bool{
	"-d": true, "--detach": true,
	"--dry-run": true,
	"-T":        true, "--no-tty": true,
	"-i": true, "--interactive": true,
	"-t": true, "--tty": true,
	"--privileged": true,
}

// parseExec strictly finds the service boundary. Unknown options fail closed:
// a future value-taking flag must not cause Vaka to wrap the flag value as if
// it were a service name.
func parseExec(args []string) (parsedExec, error) {
	parsed := parsedExec{}
	for i := 0; i < len(args); {
		tok := args[i]
		if tok == "--" {
			i++
			if i >= len(args) {
				return parsedExec{}, fmt.Errorf("compose exec: missing SERVICE after --")
			}
			parsed.service = args[i]
			i++
			parsed.command = append([]string{}, args[i:]...)
			break
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			parsed.service = tok
			i++
			parsed.command = append([]string{}, args[i:]...)
			break
		}
		if tok == "--privileged" {
			parsed.privileged = true
			parsed.prefix = append(parsed.prefix, tok)
			i++
			continue
		}
		if strings.HasPrefix(tok, "-u") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
			parsed.user = tok[2:]
			i++
			continue
		}
		if len(tok) > 2 && !strings.HasPrefix(tok, "--") && (strings.HasPrefix(tok, "-e") || strings.HasPrefix(tok, "-w")) {
			parsed.prefix = append(parsed.prefix, tok)
			i++
			continue
		}

		if flag, value, consumed, _, ok := parseValueTakingToken(args, i, execOptionsWithValue); ok {
			if value == "" {
				return parsedExec{}, fmt.Errorf("compose exec: %s requires a value", flag)
			}
			if flag == "--index" {
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 {
					return parsedExec{}, fmt.Errorf("compose exec: --index must be a positive integer, got %q", value)
				}
				parsed.index = n
			}
			if flag == "-u" || flag == "--user" {
				parsed.user = value
				i += consumed
				continue
			}
			parsed.prefix = append(parsed.prefix, args[i:i+consumed]...)
			i += consumed
			continue
		}
		if execBooleanOptions[tok] {
			parsed.prefix = append(parsed.prefix, tok)
			i++
			continue
		}
		if isExecShortBooleanCluster(tok) {
			parsed.prefix = append(parsed.prefix, tok)
			i++
			continue
		}
		return parsedExec{}, fmt.Errorf("compose exec: unknown option %q before SERVICE; upgrade Vaka if this is a new Docker Compose option", tok)
	}
	if parsed.service == "" {
		return parsedExec{}, fmt.Errorf("compose exec: missing SERVICE")
	}
	if len(parsed.command) == 0 {
		return parsedExec{}, fmt.Errorf("compose exec: missing COMMAND for service %s", parsed.service)
	}
	return parsed, nil
}

func isExecShortBooleanCluster(tok string) bool {
	if len(tok) < 3 || tok[0] != '-' || tok[1] == '-' {
		return false
	}
	for _, r := range tok[1:] {
		if !strings.ContainsRune("dTit", r) {
			return false
		}
	}
	return true
}

func secureExecInvocation(inv *ComposeInvocation, parsed parsedExec) (*ComposeInvocation, error) {
	args := append([]string{}, inv.Args[:inv.SubcommandIdx+1]...)
	args = append(args, parsed.prefix...)
	// Compose uses this only for the short-lived trusted trampoline. The
	// trampoline removes Vaka-added capabilities and restores the policy user
	// before it executes the requested command.
	args = append(args, "--user=0:0", parsed.service, vakaInitPath, "exec")
	if parsed.user != "" {
		args = append(args, "--user", parsed.user)
	}
	args = append(args, "--")
	args = append(args, parsed.command...)
	return ParseComposeInvocation(args)
}

func runSecureExec(inv *ComposeInvocation) error {
	parsed, err := parseExec(inv.PostSubcommand)
	if err != nil {
		return err
	}
	ctx := context.Background()
	project, err := resolveComposeProjectName(ctx, inv)
	if err != nil {
		return fmt.Errorf("resolve Compose project for exec: %w", err)
	}
	ds, err := newDockerServices(inv, PullNever)
	if err != nil {
		return err
	}
	inspector, ok := ds.(execTargetInspector)
	if !ok {
		return fmt.Errorf("inspect exec target: Docker service implementation does not support live container metadata")
	}
	target, err := inspector.InspectExecTarget(ctx, project, parsed.service, parsed.index)
	if err != nil {
		return err
	}
	if !target.Managed {
		return runReference(inv)
	}
	if parsed.privileged {
		return fmt.Errorf("compose exec --privileged is incompatible with Vaka-managed services; use a direct Docker command only when intentionally bypassing Vaka")
	}
	if target.RuntimeVersion != runtimeBundleVersion {
		actual := target.RuntimeVersion
		if actual == "" {
			actual = "<missing>"
		}
		return fmt.Errorf("service %s uses Vaka runtime %s, but this CLI requires %s; recreate it with `vaka up --force-recreate` before exec", parsed.service, actual, runtimeBundleVersion)
	}
	if target.RuntimeImage == "" || !target.RuntimeMounted {
		return fmt.Errorf("service %s does not use Vaka's verified read-only runtime mount; recreate it without --vaka-init-present or agent.vaka.init before exec", parsed.service)
	}
	secured, err := secureExecInvocation(inv, parsed)
	if err != nil {
		return err
	}
	return runReference(secured)
}
