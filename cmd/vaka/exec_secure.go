package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/moby/term"
	"vaka.dev/vaka/internal/runtimebundle"
	"vaka.dev/vaka/pkg/compose"
)

const (
	composeContainerNumberLabel = "com.docker.compose.container-number"
	composeConfigHashLabel      = "com.docker.compose.config-hash"
	composeOneoffLabel          = "com.docker.compose.oneoff"
)

type execTarget struct {
	ContainerID     string
	Oneoff          bool
	CanExec         bool
	Managed         bool
	LegacyManaged   bool
	RuntimeVersion  string
	RuntimeImage    string
	RuntimeMounted  bool
	ValidationError string
}

type containerInspectClient interface {
	ContainerInspect(context.Context, string) (containertypes.InspectResponse, error)
}

type execTargetInspector interface {
	InspectExecTarget(context.Context, string, string, int) (execTarget, error)
}

// InspectExecTarget resolves the same service replica selected by Compose
// exec and reads security metadata from the live container. Callers combine
// live metadata with current policy membership: neither one is sufficient on
// its own to decide that falling through to raw Compose is safe.
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
		if isComposeOneoff(ctr) {
			continue
		}
		if ctr.Labels[composeProjectLabel] != project || ctr.Labels[composeServiceLabel] != service {
			continue
		}
		if index > 0 && containerNumber(ctr) != index {
			continue
		}
		return d.inspectExecContainer(ctx, ctr)
	}
	if index > 0 {
		return execTarget{}, fmt.Errorf("service %s has no running container at index %d in Compose project %s", service, index, project)
	}
	return execTarget{}, fmt.Errorf("service %s has no running container in Compose project %s", service, project)
}

func (d *dockerServices) inspectExecContainer(ctx context.Context, summary containertypes.Summary) (execTarget, error) {
	inspector, ok := d.legacy.(containerInspectClient)
	if !ok {
		return execTarget{}, fmt.Errorf("inspect container %s: Docker client does not provide full container metadata", summary.ID)
	}
	inspect, err := inspector.ContainerInspect(ctx, summary.ID)
	if err != nil {
		return execTarget{}, fmt.Errorf("inspect selected container %s on %s: %w", summary.ID, d.targetDesc, err)
	}
	if inspect.ID != summary.ID {
		return execTarget{}, fmt.Errorf("inspect selected container %s returned different identity %s", summary.ID, inspect.ID)
	}
	labels := summary.Labels
	if inspect.Config != nil {
		labels = inspect.Config.Labels
	}
	target := execTarget{
		ContainerID:    inspect.ID,
		CanExec:        containerAcceptsExec(inspect),
		Managed:        labels[compose.ManagedLabel] == "true",
		RuntimeVersion: strings.TrimSpace(labels[compose.RuntimeVersionLabel]),
		RuntimeImage:   strings.TrimSpace(labels[compose.RuntimeImageLabel]),
	}
	if !target.Managed {
		target.LegacyManaged = legacyManagedSignature(inspect)
		return target, nil
	}
	serviceImage := strings.TrimSpace(labels[compose.ServiceImageLabel])
	if err := d.verifyManagedContainerMounts(ctx, inspect, target.RuntimeImage, serviceImage); err != nil {
		target.ValidationError = err.Error()
		return target, nil
	}
	target.RuntimeMounted = true
	return target, nil
}

func containerAcceptsExec(inspect containertypes.InspectResponse) bool {
	return inspect.State != nil && inspect.State.Running && !inspect.State.Paused && !inspect.State.Restarting
}

func legacyManagedSignature(inspect containertypes.InspectResponse) bool {
	if inspect.Config == nil || len(inspect.Config.Entrypoint) == 0 || inspect.Config.Entrypoint[0] != legacyVakaInitPath {
		return false
	}
	if !dockerUserIsRoot(inspect.Config.User) {
		return false
	}
	if inspect.Config.Labels[vakaInitLabel] == "present" {
		return true
	}
	hasRuntime := false
	for _, mounted := range inspect.Mounts {
		destination := path.Clean(mounted.Destination)
		hasRuntime = hasRuntime || destination == legacyRuntimePath
	}
	return hasRuntime
}

func dockerUserIsRoot(raw string) bool {
	user, _, _ := strings.Cut(strings.TrimSpace(raw), ":")
	return user == "" || user == "0" || strings.EqualFold(user, "root")
}

func (d *dockerServices) verifyManagedContainerMounts(ctx context.Context, inspect containertypes.InspectResponse, runtimeImage, serviceImage string) error {
	if !validDockerImageID(runtimeImage) {
		return fmt.Errorf("invalid or missing runtime image identity %q", runtimeImage)
	}
	if !validDockerImageID(serviceImage) || inspect.Image != serviceImage {
		return fmt.Errorf("container service image %q does not match inspected image label %q", inspect.Image, serviceImage)
	}

	runtimeMounts := 0
	for _, mounted := range inspect.Mounts {
		destination := mounted.Destination
		if pathsOverlap(destination, protectedRuntimePath) {
			if path.Clean(destination) != protectedRuntimePath || mounted.Type != mount.TypeImage || mounted.RW {
				return fmt.Errorf("mount %s (%s, rw=%t) overlaps protected runtime %s", destination, mounted.Type, mounted.RW, protectedRuntimePath)
			}
			runtimeMounts++
		}
		if pathsOverlap(destination, protectedPolicyPath) {
			return fmt.Errorf("unexpected mount %s (%s, rw=%t) overlaps reserved policy path %s", destination, mounted.Type, mounted.RW, protectedPolicyPath)
		}
	}
	if runtimeMounts != 1 {
		return fmt.Errorf("expected exactly one read-only image mount at %s, found %d", protectedRuntimePath, runtimeMounts)
	}
	if inspect.HostConfig == nil {
		return fmt.Errorf("container has no HostConfig mount metadata")
	}
	hostRuntimeMounts := 0
	for _, configured := range inspect.HostConfig.Mounts {
		if !pathsOverlap(configured.Target, protectedRuntimePath) {
			continue
		}
		if path.Clean(configured.Target) != protectedRuntimePath || configured.Type != mount.TypeImage || !configured.ReadOnly {
			return fmt.Errorf("configured mount %s (%s, readOnly=%t) overlaps protected runtime", configured.Target, configured.Type, configured.ReadOnly)
		}
		if configured.ImageOptions == nil || configured.ImageOptions.Subpath != runtimebundle.ImageSubpath {
			return fmt.Errorf("runtime image mount has unexpected subpath %q", imageMountSubpath(configured))
		}
		resolved, err := d.c.ImageInspect(ctx, configured.Source)
		if err != nil {
			return fmt.Errorf("resolve runtime mount source %q: %w", configured.Source, err)
		}
		if resolved.ID != runtimeImage {
			return fmt.Errorf("runtime mount source %q resolves to %q, expected %q", configured.Source, resolved.ID, runtimeImage)
		}
		hostRuntimeMounts++
	}
	if hostRuntimeMounts != 1 {
		return fmt.Errorf("expected exactly one configured runtime image mount, found %d", hostRuntimeMounts)
	}
	runtimePath, err := d.c.ContainerStatPath(ctx, inspect.ID, protectedRuntimePath)
	if err != nil {
		return fmt.Errorf("inspect literal runtime path %s in container %s: %w", protectedRuntimePath, inspect.ID, err)
	}
	if runtimePath.Mode&os.ModeSymlink != 0 {
		return fmt.Errorf("literal runtime path %s is a symbolic link", protectedRuntimePath)
	}
	if !runtimePath.Mode.IsDir() {
		return fmt.Errorf("literal runtime path %s is not a directory", protectedRuntimePath)
	}
	return nil
}

func imageMountSubpath(configured mount.Mount) string {
	if configured.ImageOptions == nil {
		return ""
	}
	return configured.ImageOptions.Subpath
}

type projectExecutionInspector interface {
	InspectProjectContainers(context.Context, string, projectContainerSelection) (map[string][]execTarget, error)
}

type projectContainerSelection struct {
	Services       map[string]bool
	IncludeOneoffs bool
}

func (d *dockerServices) InspectProjectContainers(ctx context.Context, project string, selection projectContainerSelection) (map[string][]execTarget, error) {
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
		oneoff := isComposeOneoff(ctr)
		if oneoff && !selection.IncludeOneoffs {
			continue
		}
		service := ctr.Labels[composeServiceLabel]
		if service == "" {
			continue
		}
		if selection.Services != nil && !selection.Services[service] {
			continue
		}
		target, err := d.inspectExecContainer(ctx, ctr)
		if err != nil {
			return nil, err
		}
		target.Oneoff = oneoff
		targets[service] = append(targets[service], target)
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
	service     string
	command     []string
	index       int
	prefix      []string
	user        string
	privileged  bool
	detach      bool
	interactive bool
	noTTY       bool
	tty         bool
	ttySet      bool
	noTTYSet    bool
	dryRun      bool
	dryRunSet   bool
}

var execDockerContainerFn = func(args []string) error {
	cmd := exec.Command("docker", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

var execOutputIsTerminalFn = func() bool {
	_, isTerminal := term.GetFdInfo(os.Stdout)
	return isTerminal
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
	"--no-TTY": true,
	"-i":       true, "--interactive": true,
	"-t": true, "--tty": true,
	"--privileged": true,
}

// parseExec strictly finds the service boundary. Unknown options fail closed:
// a future value-taking flag must not cause Vaka to wrap the flag value as if
// it were a service name.
func parseExec(args []string) (parsedExec, error) {
	parsed := parsedExec{interactive: true, tty: true}
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
		if strings.HasPrefix(tok, "--") {
			name, value, hasValue := strings.Cut(tok, "=")
			if hasValue && execBooleanOptions[name] {
				enabled, err := composeBoolValue(name, value)
				if err != nil {
					return parsedExec{}, err
				}
				parsed.recordExecBoolean(name, enabled)
				parsed.prefix = append(parsed.prefix, tok)
				i++
				continue
			}
		}
		if len(tok) > 3 && tok[0] == '-' && tok[1] != '-' && tok[2] == '=' && execBooleanOptions[tok[:2]] {
			enabled, err := composeBoolValue(tok[:2], tok[3:])
			if err != nil {
				return parsedExec{}, err
			}
			parsed.recordExecBoolean(tok[:2], enabled)
			parsed.prefix = append(parsed.prefix, tok)
			i++
			continue
		}
		if strings.HasPrefix(tok, "-u") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
			parsed.user = strings.TrimPrefix(tok[2:], "=")
			if parsed.user == "" {
				return parsedExec{}, fmt.Errorf("compose exec: -u requires a value")
			}
			i++
			continue
		}
		if len(tok) > 2 && !strings.HasPrefix(tok, "--") && (strings.HasPrefix(tok, "-e") || strings.HasPrefix(tok, "-w")) {
			parsed.prefix = append(parsed.prefix, tok)
			i++
			continue
		}
		if execBooleanOptions[tok] {
			parsed.recordExecBoolean(tok, true)
			parsed.prefix = append(parsed.prefix, tok)
			i++
			continue
		}
		if isExecShortBooleanCluster(tok) {
			for _, flag := range tok[1:] {
				parsed.recordExecBoolean("-"+string(flag), true)
			}
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
	if parsed.ttySet && parsed.noTTYSet {
		return parsedExec{}, fmt.Errorf("compose exec: --tty and --no-tty cannot be used together")
	}
	return parsed, nil
}

func (p *parsedExec) recordExecBoolean(flag string, enabled bool) {
	switch flag {
	case "-d", "--detach":
		p.detach = enabled
	case "-i", "--interactive":
		p.interactive = enabled
	case "-T", "--no-tty", "--no-TTY":
		p.noTTY = enabled
		p.noTTYSet = true
	case "-t", "--tty":
		p.tty = enabled
		p.ttySet = true
	case "--privileged":
		p.privileged = enabled
	case "--dry-run":
		p.dryRun = enabled
		p.dryRunSet = true
	}
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

func secureDockerExecArgs(containerID string, parsed parsedExec) ([]string, error) {
	if strings.TrimSpace(containerID) == "" {
		return nil, fmt.Errorf("managed exec target has no exact container ID")
	}
	if parsed.dryRun {
		return nil, fmt.Errorf("compose exec --dry-run is not supported for Vaka-managed services")
	}
	if reservedExecEnvironmentOverride(parsed.prefix) != "" {
		return nil, fmt.Errorf("compose exec cannot override reserved Vaka environment variable %s", reservedExecEnvironmentOverride(parsed.prefix))
	}
	args := []string{"exec"}
	for i := 0; i < len(parsed.prefix); i++ {
		tok := parsed.prefix[i]
		switch {
		case tok == "--index":
			i++
		case strings.HasPrefix(tok, "--index="):
		case isExecBooleanToken(tok):
		case isExecShortBooleanCluster(tok):
		default:
			args = append(args, tok)
		}
	}
	if parsed.detach {
		args = append(args, "-d")
	}
	if parsed.interactive {
		args = append(args, "-i")
	}
	tty := execOutputIsTerminalFn()
	if parsed.noTTYSet {
		tty = !parsed.noTTY
	}
	if parsed.ttySet {
		tty = parsed.tty
	}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, "--user=0:0", containerID, vakaInitPath, "exec")
	if parsed.user != "" {
		args = append(args, "--user", parsed.user)
	}
	args = append(args, "--")
	args = append(args, parsed.command...)
	return args, nil
}

func isExecBooleanToken(tok string) bool {
	if execBooleanOptions[tok] {
		return true
	}
	name, _, hasValue := strings.Cut(tok, "=")
	return hasValue && execBooleanOptions[name]
}

func reservedExecEnvironmentOverride(prefix []string) string {
	for i := 0; i < len(prefix); i++ {
		tok := prefix[i]
		value := ""
		switch {
		case tok == "-e" || tok == "--env":
			if i+1 < len(prefix) {
				value = prefix[i+1]
				i++
			}
		case strings.HasPrefix(tok, "--env="):
			value = strings.TrimPrefix(tok, "--env=")
		case strings.HasPrefix(tok, "-e") && len(tok) > 2:
			value = strings.TrimPrefix(strings.TrimPrefix(tok, "-e"), "=")
		}
		if name := reservedVakaEnvironmentName(value); name != "" {
			return name
		}
	}
	return ""
}

func reservedVakaEnvironmentName(value string) string {
	name, _, _ := strings.Cut(value, "=")
	name = strings.TrimSpace(name)
	if name == runtimebundle.PolicyEnvironment || name == runtimebundle.PolicyRevisionEnvironment {
		return name
	}
	return ""
}

func runSecureExec(vakaFile string, inv *ComposeInvocation) error {
	parsed, err := parseExec(inv.PostSubcommand)
	if err != nil {
		return err
	}
	ctx := context.Background()
	composeInput, err := resolveComposeInput(inv)
	if err != nil {
		return err
	}
	p, project, err := loadAndValidateResolved(vakaFile, composeInput)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("resolve Compose project for exec: no Compose project was loaded")
	}
	ds, err := newDockerServices(inv, PullNever)
	if err != nil {
		return err
	}
	inspector, ok := ds.(execTargetInspector)
	if !ok {
		return fmt.Errorf("inspect exec target: Docker service implementation does not support live container metadata")
	}
	target, err := inspector.InspectExecTarget(ctx, project.Name, parsed.service, parsed.index)
	if err != nil {
		return err
	}
	if target.LegacyManaged {
		return fmt.Errorf("service %s uses a legacy Vaka-managed container without current security metadata; recreate it with `vaka up --force-recreate` before exec", parsed.service)
	}
	if !target.Managed {
		if _, policyManaged := p.Services[parsed.service]; policyManaged {
			return fmt.Errorf("service %s is managed by %s but its live container was not created by Vaka; recreate it with `vaka up --force-recreate` before exec, or use raw `docker compose exec` only for an intentional policy bypass", parsed.service, vakaFile)
		}
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
		if target.ValidationError != "" {
			return fmt.Errorf("service %s does not use Vaka's verified runtime and policy mounts: %s; recreate it with `vaka up --force-recreate`", parsed.service, target.ValidationError)
		}
		return fmt.Errorf("service %s does not use Vaka's verified read-only runtime mount; recreate it without --vaka-init-present or agent.vaka.init before exec", parsed.service)
	}
	args, err := secureDockerExecArgs(target.ContainerID, parsed)
	if err != nil {
		return err
	}
	return execDockerContainerFn(args)
}
