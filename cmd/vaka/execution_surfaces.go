package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"

	composeformat "github.com/compose-spec/compose-go/v2/format"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/mattn/go-shellwords"
	"vaka.dev/vaka/pkg/policy"
)

const (
	protectedRuntimePath = "/opt/vaka"
	protectedPolicyPath  = "/run/secrets/vaka.yaml"
)

var runOptionsWithValue = map[string]bool{
	"--cap-add": true, "--cap-drop": true,
	"-e": true, "--env": true,
	"--env-from-file": true,
	"--entrypoint":    true,
	"-l":              true, "--label": true,
	"--labels": true,
	"--name":   true,
	"-p":       true, "--publish": true,
	"--pull": true,
	"-u":     true, "--user": true,
	"-v": true, "--volume": true, "--volumes": true,
	"-w": true, "--workdir": true,
}

var runBooleanOptions = map[string]bool{
	"--build": true,
	"-d":      true, "--detach": true,
	"--dry-run": true,
	"-i":        true, "--interactive": true,
	"--no-deps": true,
	"-T":        true, "--no-tty": true,
	"--no-TTY": true,
	"-q":       true, "--quiet": true,
	"--quiet-build":    true,
	"--quiet-pull":     true,
	"--remove-orphans": true,
	"--rm":             true,
	"-P":               true, "--service-ports": true,
	"-t": true, "--tty": true,
	"--use-aliases": true,
}

type parsedRun struct {
	service            string
	serviceIndex       int
	entrypoint         bool
	entrypointValue    string
	user               bool
	build              bool
	noDeps             bool
	dryRun             bool
	dryRunSet          bool
	pull               string
	pullSet            bool
	pullAlways         bool
	capabilityOverride bool
	volumes            []string
	publish            []string
	labels             []string
	environment        []string
	environmentFile    bool
	environmentFiles   []string
	servicePorts       bool
	quietPull          bool
	quietBuild         bool
	ttySet             bool
	noTTYSet           bool
}

func parseRun(args []string) (parsedRun, error) {
	parsed := parsedRun{serviceIndex: -1}
	for i := 0; i < len(args); {
		tok := args[i]
		if tok == "--" {
			i++
			if i >= len(args) {
				return parsedRun{}, fmt.Errorf("compose run: missing SERVICE after --")
			}
			parsed.service = args[i]
			parsed.serviceIndex = i
			break
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			parsed.service = tok
			parsed.serviceIndex = i
			break
		}
		if flag, value, consumed, _, ok := parseValueTakingToken(args, i, runOptionsWithValue); ok {
			if value == "" {
				return parsedRun{}, fmt.Errorf("compose run: %s requires a value", flag)
			}
			switch flag {
			case "--cap-add", "--cap-drop":
				parsed.capabilityOverride = true
			case "--entrypoint":
				parsed.entrypoint = true
				parsed.entrypointValue = value
			case "-u", "--user":
				parsed.user = true
			case "-e", "--env":
				parsed.environment = append(parsed.environment, value)
			case "--pull":
				parsed.pull = value
				parsed.pullSet = true
				parsed.pullAlways = value == "always"
			case "--env-from-file":
				parsed.environmentFile = true
				parsed.environmentFiles = append(parsed.environmentFiles, value)
			case "-v", "--volume", "--volumes":
				parsed.volumes = append(parsed.volumes, value)
			case "-l", "--label", "--labels":
				parsed.labels = append(parsed.labels, value)
			case "-p", "--publish":
				parsed.publish = append(parsed.publish, value)
			}
			i += consumed
			continue
		}
		if strings.HasPrefix(tok, "--") {
			name, value, hasValue := strings.Cut(tok, "=")
			if hasValue && runBooleanOptions[name] {
				enabled, err := composeBoolValue(name, value)
				if err != nil {
					return parsedRun{}, err
				}
				parsed.recordRunBoolean(name, enabled)
				i++
				continue
			}
		}
		if len(tok) > 3 && tok[0] == '-' && tok[1] != '-' && tok[2] == '=' && runBooleanOptions[tok[:2]] {
			enabled, err := composeBoolValue(tok[:2], tok[3:])
			if err != nil {
				return parsedRun{}, err
			}
			parsed.recordRunBoolean(tok[:2], enabled)
			i++
			continue
		}
		if runBooleanOptions[tok] {
			parsed.recordRunBoolean(tok, true)
			i++
			continue
		}
		if isRunShortBooleanCluster(tok) {
			for _, flag := range tok[1:] {
				parsed.recordRunBoolean("-"+string(flag), true)
			}
			i++
			continue
		}
		if len(tok) > 2 && tok[0] == '-' && tok[1] != '-' {
			flag := tok[:2]
			value := tok[2:]
			switch flag {
			case "-p":
				parsed.publish = append(parsed.publish, strings.TrimPrefix(value, "="))
				i++
				continue
			case "-w":
				i++
				continue
			case "-e":
				parsed.environment = append(parsed.environment, strings.TrimPrefix(value, "="))
				i++
				continue
			case "-l":
				parsed.labels = append(parsed.labels, value)
				i++
				continue
			case "-u":
				parsed.user = true
				i++
				continue
			case "-v":
				parsed.volumes = append(parsed.volumes, value)
				i++
				continue
			}
		}
		return parsedRun{}, fmt.Errorf("compose run: unknown option %q before SERVICE; upgrade Vaka if this is a new Docker Compose option", tok)
	}
	if parsed.service == "" {
		return parsedRun{}, fmt.Errorf("compose run: missing SERVICE")
	}
	if parsed.servicePorts && len(parsed.publish) > 0 {
		return parsedRun{}, fmt.Errorf("compose run: --service-ports and --publish are incompatible")
	}
	if parsed.ttySet && parsed.noTTYSet {
		return parsedRun{}, fmt.Errorf("compose run: --tty and --no-tty cannot be used together")
	}
	if parsed.entrypoint {
		if _, err := shellwords.Parse(parsed.entrypointValue); err != nil {
			return parsedRun{}, fmt.Errorf("compose run: parse --entrypoint: %w", err)
		}
	}
	for _, raw := range parsed.publish {
		if _, err := composetypes.ParsePortConfig(raw); err != nil {
			return parsedRun{}, fmt.Errorf("compose run: parse published port %q: %w", raw, err)
		}
	}
	for _, raw := range parsed.volumes {
		if _, err := composeformat.ParseVolume(raw); err != nil {
			return parsedRun{}, fmt.Errorf("compose run: parse volume %q: %w", raw, err)
		}
	}
	for _, label := range parsed.labels {
		key, _, ok := strings.Cut(label, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return parsedRun{}, fmt.Errorf("compose run: label must be set as KEY=VALUE, got %q", label)
		}
	}
	for _, file := range parsed.environmentFiles {
		f, err := os.Open(file)
		if err != nil {
			return parsedRun{}, fmt.Errorf("compose run: open --env-from-file %q: %w", file, err)
		}
		if err := f.Close(); err != nil {
			return parsedRun{}, fmt.Errorf("compose run: close --env-from-file %q: %w", file, err)
		}
	}
	return parsed, nil
}

func (p *parsedRun) recordRunBoolean(flag string, enabled bool) {
	switch flag {
	case "--build":
		p.build = enabled
	case "--dry-run":
		p.dryRun = enabled
		p.dryRunSet = true
	case "--no-deps":
		p.noDeps = enabled
	case "-P", "--service-ports":
		p.servicePorts = enabled
	case "--quiet-pull":
		p.quietPull = enabled
	case "--quiet-build":
		p.quietBuild = enabled
	case "-t", "--tty":
		p.ttySet = true
	case "-T", "--no-tty", "--no-TTY":
		p.noTTYSet = true
	}
}

func selectRunServices(project *composetypes.Project, parsed parsedRun) (*composetypes.Project, error) {
	targets := []string{parsed.service}
	enabled, err := project.WithServicesEnabled(targets...)
	if err != nil {
		return nil, err
	}
	if parsed.noDeps {
		return enabled.WithSelectedServices(targets, composetypes.IgnoreDependencies)
	}
	return enabled.WithSelectedServices(targets)
}

func isRunShortBooleanCluster(tok string) bool {
	if len(tok) < 3 || tok[0] != '-' || tok[1] == '-' {
		return false
	}
	for _, r := range tok[1:] {
		if !strings.ContainsRune("diTqPt", r) {
			return false
		}
	}
	return true
}

func validateRunInvocation(inv *ComposeInvocation, p *policy.ServicePolicy) error {
	parsed, err := parseRun(inv.PostSubcommand)
	if err != nil {
		return err
	}
	if _, managed := p.Services[parsed.service]; !managed {
		return nil
	}
	if parsed.entrypoint {
		return fmt.Errorf("compose run --entrypoint bypasses Vaka's policy entrypoint for managed service %s; pass the command after SERVICE instead", parsed.service)
	}
	if parsed.user {
		return fmt.Errorf("compose run --user is incompatible with Vaka's privileged startup and identity restoration for managed service %s", parsed.service)
	}
	if parsed.capabilityOverride {
		return fmt.Errorf("compose run --cap-add/--cap-drop cannot safely alter Vaka's temporary capability plan for managed service %s; declare capabilities in Compose instead", parsed.service)
	}
	for _, value := range parsed.environment {
		if name := reservedVakaEnvironmentName(value); name != "" {
			return fmt.Errorf("compose run cannot override reserved Vaka environment variable %s for managed service %s", name, parsed.service)
		}
	}
	if parsed.environmentFile {
		return fmt.Errorf("compose run --env-from-file is not supported for Vaka-managed service %s because it could override reserved policy variables", parsed.service)
	}
	for _, raw := range parsed.volumes {
		volume, err := composeformat.ParseVolume(raw)
		if err != nil {
			return fmt.Errorf("compose run: parse volume %q: %w", raw, err)
		}
		target := path.Clean(volume.Target)
		if protectedPathOverlap(target) {
			return fmt.Errorf("compose run volume %q overlaps Vaka's protected runtime or policy mounts for managed service %s", raw, parsed.service)
		}
	}
	for _, label := range parsed.labels {
		key := label
		if before, _, ok := strings.Cut(label, "="); ok {
			key = before
		}
		key = strings.TrimSpace(key)
		if strings.HasPrefix(key, "agent.vaka.") || strings.HasPrefix(key, "com.docker.compose.") {
			return fmt.Errorf("compose run label %q overrides Vaka security metadata for managed service %s", label, parsed.service)
		}
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	left, right = path.Clean(left), path.Clean(right)
	return pathContains(left, right) || pathContains(right, left)
}

func pathContains(parent, child string) bool {
	if parent == child {
		return true
	}
	if parent == "/" {
		return strings.HasPrefix(child, "/")
	}
	return strings.HasPrefix(child, parent+"/")
}

func protectedPathOverlap(target string) bool {
	return pathsOverlap(target, protectedRuntimePath) || pathsOverlap(target, protectedPolicyPath)
}

func validateManagedExecutionSurfaces(p *policy.ServicePolicy, project *composetypes.Project) error {
	for name := range p.Services {
		svc, ok := project.Services[name]
		if !ok {
			continue
		}
		if err := validateServiceExecutionSurfaces(name, svc); err != nil {
			return err
		}
	}
	return nil
}

func validateServiceExecutionSurfaces(name string, svc composetypes.ServiceConfig) error {
	if svc.Labels[vakaInitLabel] == "present" {
		return fmt.Errorf("service %s: label %s=present is not supported: the exec security boundary requires Vaka's verified read-only runtime mount", name, vakaInitLabel)
	}
	if err := validateLifecycleHooks(name, svc, true, true); err != nil {
		return err
	}
	if _, ok := svc.Extensions["x-develop"]; ok {
		return fmt.Errorf("service %s: deprecated x-develop is not supported on Vaka-managed services because Compose can execute watch actions outside vaka-init; migrate it to develop", name)
	}
	if svc.Develop != nil {
		for _, trigger := range svc.Develop.Watch {
			switch trigger.Action {
			case composetypes.WatchActionSync, composetypes.WatchActionSyncRestart, composetypes.WatchActionSyncExec, composetypes.WatchActionRebuild:
				return fmt.Errorf("service %s: develop.watch action %s is not supported on Vaka-managed services because Compose can execute file-deletion or hook commands outside vaka-init", name, trigger.Action)
			case composetypes.WatchActionRestart:
				// Restart preserves the verified container configuration and does
				// not synchronize files or create an unwrapped exec process.
			default:
				return fmt.Errorf("service %s: develop.watch action %q has not been reviewed for Vaka's process security boundary", name, trigger.Action)
			}
			if trigger.Target != "" && protectedPathOverlap(trigger.Target) {
				return fmt.Errorf("service %s: develop.watch target %q overlaps Vaka's protected runtime or policy mounts", name, trigger.Target)
			}
		}
	}
	for _, volume := range svc.Volumes {
		if protectedPathOverlap(volume.Target) {
			return fmt.Errorf("service %s: volume target %q overlaps Vaka's protected runtime or policy mount", name, volume.Target)
		}
	}
	for _, config := range svc.Configs {
		target := config.Target
		if target == "" {
			target = "/" + config.Source
		}
		if protectedPathOverlap(target) {
			return fmt.Errorf("service %s: config target %q overlaps Vaka's protected runtime or policy mount", name, target)
		}
	}
	for _, secret := range svc.Secrets {
		target := secret.Target
		if target == "" {
			target = "/run/secrets/" + secret.Source
		}
		if protectedPathOverlap(target) {
			return fmt.Errorf("service %s: secret target %q overlaps Vaka's protected runtime or policy mount", name, target)
		}
	}
	for _, tmpfs := range svc.Tmpfs {
		target, _, _ := strings.Cut(tmpfs, ":")
		if protectedPathOverlap(target) {
			return fmt.Errorf("service %s: tmpfs target %q overlaps Vaka's protected runtime or policy mount", name, target)
		}
	}
	if len(svc.VolumesFrom) > 0 {
		return fmt.Errorf("service %s: volumes_from is not supported on Vaka-managed services because inherited mount targets cannot be verified before creation", name)
	}
	for _, device := range svc.Devices {
		if protectedPathOverlap(device.Target) {
			return fmt.Errorf("service %s: device target %q overlaps Vaka's protected runtime or policy mount", name, device.Target)
		}
	}
	return nil
}

func validateLifecycleHooks(name string, svc composetypes.ServiceConfig, preStop, postStart bool) error {
	if preStop && len(svc.PreStop) > 0 {
		return fmt.Errorf("service %s: pre_stop hooks are not supported on Vaka-managed services because Compose executes them outside vaka-init", name)
	}
	if postStart && len(svc.PostStart) > 0 {
		return fmt.Errorf("service %s: post_start hooks are not supported on Vaka-managed services because Compose executes them outside vaka-init", name)
	}
	return nil
}

type lifecycleExecution struct {
	resumes   bool
	preStop   bool
	postStart bool
}

func lifecycleExecutionFor(subcommand string) lifecycleExecution {
	switch subcommand {
	case "start":
		return lifecycleExecution{resumes: true, postStart: true}
	case "restart":
		return lifecycleExecution{resumes: true, preStop: true, postStart: true}
	case "unpause":
		return lifecycleExecution{resumes: true}
	case "stop", "down", "rm":
		return lifecycleExecution{preStop: true}
	default:
		return lifecycleExecution{}
	}
}

func validateReferenceExecutionSurfaces(vakaFile string, inv *ComposeInvocation) error {
	input, err := resolveComposeInput(inv)
	if err != nil {
		return err
	}
	p, project, err := loadAndValidateResolved(vakaFile, input)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("load compose project: no Compose project was loaded")
	}
	selected, err := referenceValidationServices(project, inv)
	if err != nil {
		return err
	}
	ds, err := newDockerServices(inv, PullNever)
	if err != nil {
		return err
	}
	inspector, ok := ds.(projectExecutionInspector)
	if !ok {
		return fmt.Errorf("inspect Compose project: Docker service implementation does not support live container metadata")
	}
	inspection := projectContainerSelection{Services: selected}
	if inv.Subcommand == "down" {
		removeOrphans, err := effectiveDownRemoveOrphans(project, inv)
		if err != nil {
			return err
		}
		inspection.IncludeOneoffs = removeOrphans
	}
	live, err := inspector.InspectProjectContainers(context.Background(), project.Name, inspection)
	if err != nil {
		return err
	}
	execution := lifecycleExecutionFor(inv.Subcommand)
	for name, targets := range live {
		if selected != nil && !selected[name] {
			continue
		}
		_, policyManaged := p.Services[name]
		liveManaged := targetsContainVakaRuntime(targets)
		if (policyManaged || liveManaged) && (execution.preStop || execution.postStart) {
			if svc, ok := project.Services[name]; ok {
				if err := validateLifecycleHooks(name, svc, execution.preStop, execution.postStart); err != nil {
					return err
				}
			}
		}
		if !execution.resumes {
			continue
		}
		if err := validateLifecycleResumeTargets(name, vakaFile, inv.Subcommand, policyManaged, targets); err != nil {
			return err
		}
	}
	return nil
}

func effectiveDownRemoveOrphans(project *composetypes.Project, inv *ComposeInvocation) (bool, error) {
	state, err := scanComposeBoolOption(inv.PostSubcommand, "--remove-orphans", "")
	if err != nil {
		return false, err
	}
	if state.present {
		return state.enabled, nil
	}
	return composeEnvironmentBool(project.Environment["COMPOSE_REMOVE_ORPHANS"]), nil
}

func targetsContainVakaRuntime(targets []execTarget) bool {
	for _, target := range targets {
		if target.Managed || target.LegacyManaged {
			return true
		}
	}
	return false
}

func validateLifecycleResumeTargets(name, vakaFile, subcommand string, policyManaged bool, targets []execTarget) error {
	hasVakaRuntime := false
	hasOrdinaryRuntime := false
	for _, target := range targets {
		if target.Managed || target.LegacyManaged {
			hasVakaRuntime = true
		} else {
			hasOrdinaryRuntime = true
		}
	}
	if policyManaged && hasOrdinaryRuntime {
		return fmt.Errorf("service %s is managed by %s but its live container was not created by Vaka; recreate it with `vaka up --force-recreate` before %s, or use raw `docker compose %s` only for an intentional policy bypass", name, vakaFile, subcommand, subcommand)
	}
	if hasVakaRuntime && hasOrdinaryRuntime {
		return fmt.Errorf("service %s has a mix of Vaka-managed and ordinary live containers; recreate all replicas with `vaka up --force-recreate` before %s", name, subcommand)
	}
	for _, target := range targets {
		if !target.Managed && !target.LegacyManaged {
			continue
		}
		if target.LegacyManaged || target.RuntimeVersion != runtimeBundleVersion || target.RuntimeImage == "" || !target.RuntimeMounted {
			return fmt.Errorf("service %s uses an older or mutable Vaka runtime; recreate it with `vaka up --force-recreate` before %s", name, subcommand)
		}
	}
	return nil
}

func referenceValidationServices(project *composetypes.Project, inv *ComposeInvocation) (map[string]bool, error) {
	valueOptions := map[string]bool{}
	booleanOptions := map[string]bool{"--dry-run": true}
	shortBooleans := ""
	rmi := ""
	switch inv.Subcommand {
	case "start":
		valueOptions["--wait-timeout"] = true
		booleanOptions["--wait"] = true
	case "restart":
		valueOptions["-t"] = true
		valueOptions["--timeout"] = true
		booleanOptions["--no-deps"] = true
	case "stop":
		valueOptions["-t"] = true
		valueOptions["--timeout"] = true
	case "rm":
		booleanOptions["-a"] = true
		booleanOptions["--all"] = true
		booleanOptions["-f"] = true
		booleanOptions["--force"] = true
		booleanOptions["-s"] = true
		booleanOptions["--stop"] = true
		booleanOptions["-v"] = true
		booleanOptions["--volumes"] = true
		shortBooleans = "afsv"
	case "down":
		valueOptions["-t"] = true
		valueOptions["--timeout"] = true
		valueOptions["--rmi"] = true
		booleanOptions["--remove-orphans"] = true
		booleanOptions["-v"] = true
		booleanOptions["--volume"] = true
		booleanOptions["--volumes"] = true
		shortBooleans = "v"
	case "unpause":
	default:
		return nil, nil
	}

	var targets []string
	for i := 0; i < len(inv.PostSubcommand); {
		tok := inv.PostSubcommand[i]
		if tok == "--" {
			targets = append(targets, inv.PostSubcommand[i+1:]...)
			break
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			targets = append(targets, tok)
			i++
			continue
		}
		if flag, value, consumed, _, ok := parseValueTakingToken(inv.PostSubcommand, i, valueOptions); ok {
			if value == "" {
				return nil, fmt.Errorf("compose %s: %s requires a value", inv.Subcommand, flag)
			}
			if flag == "-t" || flag == "--timeout" || flag == "--wait-timeout" {
				if _, err := strconv.Atoi(value); err != nil {
					return nil, fmt.Errorf("compose %s: %s requires an integer, got %q", inv.Subcommand, flag, value)
				}
			}
			if flag == "--rmi" {
				rmi = value
			}
			i += consumed
			continue
		}
		if (inv.Subcommand == "restart" || inv.Subcommand == "stop" || inv.Subcommand == "down") && len(tok) > 2 && tok[0] == '-' && tok[1] == 't' {
			if _, err := strconv.Atoi(tok[2:]); err != nil {
				return nil, fmt.Errorf("compose %s: -t requires an integer, got %q", inv.Subcommand, tok[2:])
			}
			i++
			continue
		}
		if booleanOptions[tok] {
			i++
			continue
		}
		if strings.HasPrefix(tok, "--") {
			name, value, hasValue := strings.Cut(tok, "=")
			if booleanOptions[name] && hasValue {
				if _, err := composeBoolValue(name, value); err != nil {
					return nil, err
				}
				i++
				continue
			}
		}
		if shortBooleans != "" && len(tok) > 2 && tok[0] == '-' && tok[1] != '-' {
			cluster, value, hasValue := strings.Cut(tok[1:], "=")
			valid := true
			for _, flag := range cluster {
				if !strings.ContainsRune(shortBooleans, flag) {
					valid = false
					break
				}
			}
			if valid {
				if hasValue {
					if _, err := composeBoolValue("-"+cluster[len(cluster)-1:], value); err != nil {
						return nil, err
					}
				}
				i++
				continue
			}
		}
		return nil, fmt.Errorf("compose %s: unknown option %q before lifecycle validation; upgrade Vaka if this is a new Docker Compose option", inv.Subcommand, tok)
	}
	if inv.Subcommand == "down" && rmi != "" && rmi != "all" && rmi != "local" {
		return nil, fmt.Errorf(`compose down: --rmi must be "all" or "local", got %q`, rmi)
	}
	if len(targets) == 0 {
		// Compose operates on active services when no targets are given. Live
		// containers from inactive profiles or obsolete project definitions are
		// not execution targets and must not block an unrelated lifecycle action.
		out := make(map[string]bool, len(project.Services))
		for name := range project.Services {
			out[name] = true
		}
		return out, nil
	}
	enabled, err := project.WithServicesEnabled(targets...)
	if err != nil {
		return nil, err
	}
	var selected *composetypes.Project
	if inv.Subcommand == "restart" {
		noDeps, err := composeBoolOptionEnabled(inv.PostSubcommand, "--no-deps", "")
		if err != nil {
			return nil, err
		}
		if noDeps {
			selected, err = enabled.WithSelectedServices(targets, composetypes.IgnoreDependencies)
		} else {
			// Compose restart follows reverse restart:true edges. Ordinary
			// dependencies are not restarted, while dependents can be selected
			// transitively when every connecting edge opts in.
			for name, svc := range enabled.Services {
				for dependency, config := range svc.DependsOn {
					if !config.Restart {
						delete(svc.DependsOn, dependency)
					}
				}
				enabled.Services[name] = svc
			}
			selected, err = enabled.WithSelectedServices(targets, composetypes.IncludeDependents)
		}
	} else if inv.Subcommand == "start" {
		selected, err = enabled.WithSelectedServices(targets)
	} else if inv.Subcommand == "down" {
		// Compose's config-backed down path prunes the project to explicit
		// service arguments before backend traversal. Untargeted down is handled
		// above; targeted down must not let an untouched dependent block safe
		// containment of the requested service.
		selected, err = enabled.WithSelectedServices(targets, composetypes.IgnoreDependencies)
	} else {
		selected, err = enabled.WithSelectedServices(targets, composetypes.IgnoreDependencies)
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(selected.Services))
	for name := range selected.Services {
		out[name] = true
	}
	return out, nil
}

func referenceRequiresExecutionValidation(inv *ComposeInvocation) (bool, error) {
	switch inv.Subcommand {
	case "start", "restart", "stop", "down", "unpause":
		return true, nil
	case "rm":
		return composeBoolOptionEnabled(inv.PostSubcommand, "--stop", "s")
	default:
		return false, nil
	}
}

type composeBoolOptionState struct {
	present bool
	enabled bool
}

func composeBoolOptionEnabled(args []string, long, short string) (bool, error) {
	state, err := scanComposeBoolOption(args, long, short)
	return state.enabled, err
}

func scanComposeBoolOption(args []string, long, short string) (composeBoolOptionState, error) {
	state := composeBoolOptionState{}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == long || (short != "" && arg == "-"+short) {
			state.present = true
			state.enabled = true
			continue
		}
		if value, ok := strings.CutPrefix(arg, long+"="); ok {
			enabled, err := composeBoolValue(long, value)
			if err != nil {
				return composeBoolOptionState{}, err
			}
			state.present = true
			state.enabled = enabled
			continue
		}
		if short != "" && len(arg) > 2 && arg[0] == '-' && arg[1] != '-' {
			cluster := arg[1:]
			if value, ok := strings.CutPrefix(cluster, short+"="); ok {
				enabled, err := composeBoolValue("-"+short, value)
				if err != nil {
					return composeBoolOptionState{}, err
				}
				state.present = true
				state.enabled = enabled
				continue
			}
			if marker := short + "="; strings.Contains(cluster, marker) {
				enabled, err := composeBoolValue("-"+short, cluster[strings.Index(cluster, marker)+len(marker):])
				if err != nil {
					return composeBoolOptionState{}, err
				}
				state.present = true
				state.enabled = enabled
				continue
			}
			if strings.Contains(cluster, short) {
				state.present = true
				state.enabled = true
			}
		}
	}
	return state, nil
}

// composeBoolValue follows the values accepted by Compose's pflag booleans
// for the forms Vaka must interpret. Vaka must reject malformed values before
// consuming an option; otherwise Compose never gets a chance to validate it.
func composeBoolValue(option, value string) (bool, error) {
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("compose option %s has invalid boolean value %q", option, value)
	}
	return enabled, nil
}
