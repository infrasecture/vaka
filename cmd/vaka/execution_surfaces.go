package main

import (
	"context"
	"fmt"
	"path"
	"strings"

	composeformat "github.com/compose-spec/compose-go/v2/format"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"vaka.dev/vaka/pkg/policy"
)

var runOptionsWithValue = map[string]bool{
	"--cap-add": true, "--cap-drop": true,
	"-e": true, "--env": true,
	"--env-from-file": true,
	"--entrypoint":    true,
	"-l":              true, "--label": true,
	"--name": true,
	"-p":     true, "--publish": true,
	"--pull": true,
	"-u":     true, "--user": true,
	"-v": true, "--volume": true,
	"-w": true, "--workdir": true,
}

var runBooleanOptions = map[string]bool{
	"--build": true,
	"-d":      true, "--detach": true,
	"--dry-run": true,
	"-i":        true, "--interactive": true,
	"--no-deps": true,
	"-T":        true, "--no-tty": true,
	"-q": true, "--quiet": true,
	"--quiet-build":    true,
	"--quiet-pull":     true,
	"--remove-orphans": true,
	"--rm":             true,
	"-P":               true, "--service-ports": true,
	"--use-aliases": true,
}

type parsedRun struct {
	service            string
	entrypoint         bool
	user               bool
	capabilityOverride bool
	volumes            []string
	labels             []string
}

func parseRun(args []string) (parsedRun, error) {
	var parsed parsedRun
	for i := 0; i < len(args); {
		tok := args[i]
		if tok == "--" {
			i++
			if i >= len(args) {
				return parsedRun{}, fmt.Errorf("compose run: missing SERVICE after --")
			}
			parsed.service = args[i]
			break
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			parsed.service = tok
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
			case "-u", "--user":
				parsed.user = true
			case "-v", "--volume":
				parsed.volumes = append(parsed.volumes, value)
			case "-l", "--label":
				parsed.labels = append(parsed.labels, value)
			}
			i += consumed
			continue
		}
		if runBooleanOptions[tok] || isRunShortBooleanCluster(tok) {
			i++
			continue
		}
		if len(tok) > 2 && tok[0] == '-' && tok[1] != '-' {
			flag := tok[:2]
			value := tok[2:]
			switch flag {
			case "-e", "-p", "-w":
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
	return parsed, nil
}

func isRunShortBooleanCluster(tok string) bool {
	if len(tok) < 3 || tok[0] != '-' || tok[1] == '-' {
		return false
	}
	for _, r := range tok[1:] {
		if !strings.ContainsRune("diTqP", r) {
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
	for _, raw := range parsed.volumes {
		volume, err := composeformat.ParseVolume(raw)
		if err != nil {
			return fmt.Errorf("compose run: parse volume %q: %w", raw, err)
		}
		target := path.Clean(volume.Target)
		if pathsOverlap(target, "/opt/vaka") || pathsOverlap(target, "/run/secrets") {
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
	if len(svc.PostStart) > 0 {
		return fmt.Errorf("service %s: post_start hooks are not supported on Vaka-managed services because Compose executes them outside vaka-init", name)
	}
	if len(svc.PreStop) > 0 {
		return fmt.Errorf("service %s: pre_stop hooks are not supported on Vaka-managed services because Compose executes them outside vaka-init", name)
	}
	if svc.Develop != nil {
		for _, trigger := range svc.Develop.Watch {
			switch trigger.Action {
			case composetypes.WatchActionSync, composetypes.WatchActionSyncRestart, composetypes.WatchActionSyncExec:
				return fmt.Errorf("service %s: develop.watch action %s is not supported on Vaka-managed services because Compose can execute file-deletion or hook commands outside vaka-init", name, trigger.Action)
			}
			if trigger.Target != "" && (pathsOverlap(trigger.Target, "/opt/vaka") || pathsOverlap(trigger.Target, "/run/secrets")) {
				return fmt.Errorf("service %s: develop.watch target %q overlaps Vaka's protected runtime or policy mounts", name, trigger.Target)
			}
		}
	}
	return nil
}

func validateReferenceExecutionSurfaces(_ string, inv *ComposeInvocation) error {
	input, err := resolveComposeInput(inv)
	if err != nil {
		return err
	}
	opts, err := newComposeProjectOptions(input, false)
	if err != nil {
		return fmt.Errorf("compose project options: %w", err)
	}
	project, err := opts.LoadProject(context.Background())
	if err != nil {
		return fmt.Errorf("load compose project: %w", err)
	}
	ds, err := newDockerServices(inv, PullNever)
	if err != nil {
		return err
	}
	inspector, ok := ds.(projectExecutionInspector)
	if !ok {
		return fmt.Errorf("inspect Compose project: Docker service implementation does not support live container metadata")
	}
	live, err := inspector.InspectManagedProject(context.Background(), project.Name)
	if err != nil {
		return err
	}
	for name, targets := range live {
		if inv.Subcommand != "unpause" {
			if svc, ok := project.Services[name]; ok {
				if err := validateServiceExecutionSurfaces(name, svc); err != nil {
					return err
				}
			}
		}
		if inv.Subcommand == "start" || inv.Subcommand == "restart" || inv.Subcommand == "unpause" {
			for _, target := range targets {
				if target.RuntimeVersion != runtimeBundleVersion || target.RuntimeImage == "" || !target.RuntimeMounted {
					return fmt.Errorf("service %s uses an older or mutable Vaka runtime; recreate it with `vaka up --force-recreate` before %s", name, inv.Subcommand)
				}
			}
		}
	}
	return nil
}

func referenceRequiresExecutionValidation(verb string) bool {
	switch verb {
	case "start", "restart", "stop", "down", "unpause":
		return true
	default:
		return false
	}
}
