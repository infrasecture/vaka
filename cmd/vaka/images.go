package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

var errImageNotFound = errors.New("image not found")

// DockerServices is the interface for all Docker daemon interactions in vaka.
// A single implementation is created per runFull invocation; a test double can
// replace it entirely.
type DockerServices interface {
	// EnsureImage inspects ref locally and pulls it if absent.
	EnsureImage(ctx context.Context, ref string) error
	// ImageExists returns true if ref is available locally. Transport errors
	// other than NotFound are propagated.
	ImageExists(ctx context.Context, ref string) (bool, error)
	// ResolveRuntime resolves runtime metadata needed by vaka:
	// effective entrypoint/command vectors and image-level USER fallback.
	ResolveRuntime(ctx context.Context, svcName string, svc composetypes.ServiceConfig) (ResolvedRuntime, error)
}

// ResolvedRuntime is resolved service runtime metadata from compose + image.
type ResolvedRuntime struct {
	Entrypoint []string
	Command    []string
	// ImageUser is the image config USER value. Compose `service.user` is
	// intentionally not folded into this field so callers can apply explicit
	// precedence rules (compose user first, image fallback second).
	ImageUser string
}

type dockerExecFn func(ctx context.Context, dockerGlobalArgs []string, args []string, stdout, stderr io.Writer) error
type dockerOutputFn func(ctx context.Context, dockerGlobalArgs []string, args []string) ([]byte, []byte, error)

// dockerServices is the production DockerServices backed by the Docker CLI.
// Using the CLI guarantees context/daemon parity with compose invocations.
type dockerServices struct {
	dockerGlobalArgs []string
	targetDesc       string
	execFn           dockerExecFn
	outputFn         dockerOutputFn
}

// NewDockerServices creates DockerServices for one vaka invocation. Docker
// target precedence is:
//   - explicit compose/global --context flag
//   - DOCKER_CONTEXT environment
//   - DOCKER_HOST/default Docker context
func NewDockerServices(args []string) (DockerServices, error) {
	dockerGlobals := []string{}
	if ctxName := dockerContextFromArgs(args); ctxName != "" {
		dockerGlobals = append(dockerGlobals, "--context", ctxName)
	}
	return &dockerServices{
		dockerGlobalArgs: dockerGlobals,
		targetDesc:       dockerTargetDescription(args),
		execFn:           runDockerCommand,
		outputFn:         runDockerCommandOutput,
	}, nil
}

// dockerContextFromArgs returns the docker context selected via compose global
// flags. The last occurrence wins. Returns empty when unset.
func dockerContextFromArgs(args []string) string {
	return composeGlobalValue(args, "--context", "-c")
}

func dockerTargetDescription(args []string) string {
	if ctxName := dockerContextFromArgs(args); ctxName != "" {
		return fmt.Sprintf("context %q (from --context)", ctxName)
	}
	if ctxName := strings.TrimSpace(os.Getenv("DOCKER_CONTEXT")); ctxName != "" {
		return fmt.Sprintf("context %q (from DOCKER_CONTEXT)", ctxName)
	}
	if host := strings.TrimSpace(os.Getenv("DOCKER_HOST")); host != "" {
		return fmt.Sprintf("daemon %q (from DOCKER_HOST)", host)
	}
	return "default Docker context"
}

func runDockerCommand(ctx context.Context, dockerGlobalArgs []string, args []string, stdout, stderr io.Writer) error {
	cmdArgs := append(append([]string{}, dockerGlobalArgs...), args...)
	c := exec.CommandContext(ctx, "docker", cmdArgs...)
	c.Stdout = stdout
	c.Stderr = stderr
	return c.Run()
}

func runDockerCommandOutput(ctx context.Context, dockerGlobalArgs []string, args []string) ([]byte, []byte, error) {
	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	err := runDockerCommand(ctx, dockerGlobalArgs, args, &outBuf, &errBuf)
	return outBuf.Bytes(), errBuf.Bytes(), err
}

func dockerErr(runErr error, stderr, stdout []byte) string {
	if msg := strings.TrimSpace(string(stderr)); msg != "" {
		return msg
	}
	if msg := strings.TrimSpace(string(stdout)); msg != "" {
		return msg
	}
	if runErr != nil {
		return runErr.Error()
	}
	return "unknown docker error"
}

func isNoSuchImage(stdout, stderr []byte) bool {
	msg := strings.ToLower(string(stdout) + "\n" + string(stderr))
	return strings.Contains(msg, "no such image")
}

// ImageExists returns true if ref is present in the local image store.
func (d *dockerServices) ImageExists(ctx context.Context, ref string) (bool, error) {
	stdout, stderr, err := d.outputFn(ctx, d.dockerGlobalArgs, []string{"image", "inspect", ref})
	if err == nil {
		return true, nil
	}
	if isNoSuchImage(stdout, stderr) {
		return false, nil
	}
	return false, fmt.Errorf("inspect %s on %s: %s", ref, d.targetDesc, dockerErr(err, stderr, stdout))
}

// EnsureImage inspects ref locally; pulls it if absent.
func (d *dockerServices) EnsureImage(ctx context.Context, ref string) error {
	ok, err := d.ImageExists(ctx, ref)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if err := d.execFn(ctx, d.dockerGlobalArgs, []string{"pull", ref}, os.Stderr, os.Stderr); err != nil {
		return fmt.Errorf("failed to pull %s on %s — check network connectivity or use --vaka-init-present if binaries are baked into the image: %w", ref, d.targetDesc, err)
	}
	return nil
}

type inspectedImageConfig struct {
	Entrypoint []string `json:"Entrypoint"`
	Cmd        []string `json:"Cmd"`
	User       string   `json:"User"`
}

func (d *dockerServices) inspectImageConfig(ctx context.Context, ref string) (*inspectedImageConfig, error) {
	stdout, stderr, err := d.outputFn(ctx, d.dockerGlobalArgs, []string{"image", "inspect", "--format", "{{json .Config}}", ref})
	if err != nil {
		if isNoSuchImage(stdout, stderr) {
			return nil, errImageNotFound
		}
		return nil, fmt.Errorf("inspect %q on %s: %s", ref, d.targetDesc, dockerErr(err, stderr, stdout))
	}
	raw := strings.TrimSpace(string(stdout))
	if raw == "" || raw == "null" {
		return nil, nil
	}
	var cfg inspectedImageConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("decode image config for %q: %w", ref, err)
	}
	return &cfg, nil
}

// ResolveRuntime resolves effective runtime metadata for svc, following
// Docker/Compose semantics:
//
//   - compose entrypoint set: resolved pair is (compose.Entrypoint, compose.Command).
//     Docker resets CMD to empty when ENTRYPOINT is overridden, so a compose
//     entrypoint without command legitimately yields an empty command.
//   - compose entrypoint empty, command set: the image's ENTRYPOINT is preserved
//     (common pattern: app image defines ENTRYPOINT, compose overrides args).
//   - both empty: both come from the image's Dockerfile defaults.
//
// For user restoration, image Config.User is also resolved when compose
// service.user is unset, so image inspection is performed when either
// entrypoint or user fallback requires it.
func (d *dockerServices) ResolveRuntime(ctx context.Context, svcName string, svc composetypes.ServiceConfig) (ResolvedRuntime, error) {
	resolved := ResolvedRuntime{
		Entrypoint: svc.Entrypoint,
		Command:    svc.Command,
	}

	needImageEntrypoint := len(svc.Entrypoint) == 0
	needImageUser := strings.TrimSpace(svc.User) == ""
	needInspect := needImageEntrypoint || needImageUser
	if !needInspect {
		return resolved, nil
	}

	if svc.Image == "" {
		return ResolvedRuntime{}, fmt.Errorf(
			"service %s: cannot resolve image defaults without image: (needed for %s)",
			svcName, missingRuntimeFieldsHint(needImageEntrypoint, needImageUser),
		)
	}

	cfg, err := d.inspectImageConfig(ctx, svc.Image)
	if err != nil {
		if errors.Is(err, errImageNotFound) {
			return ResolvedRuntime{}, fmt.Errorf(
				"service %s: image %q not available locally on %s — pull/build it first, or set compose user/entrypoint so image defaults are not needed",
				svcName, svc.Image, d.targetDesc,
			)
		}
		return ResolvedRuntime{}, fmt.Errorf("service %s: %w", svcName, err)
	}
	if cfg == nil {
		return ResolvedRuntime{}, fmt.Errorf("service %s: image %q has no Config", svcName, svc.Image)
	}

	if needImageEntrypoint {
		resolved.Entrypoint = cfg.Entrypoint
		if len(resolved.Command) == 0 {
			resolved.Command = cfg.Cmd
		}
	}
	if needImageUser {
		resolved.ImageUser = cfg.User
	}
	return resolved, nil
}

func missingRuntimeFieldsHint(needImageEntrypoint, needImageUser bool) string {
	switch {
	case needImageEntrypoint && needImageUser:
		return "entrypoint/cmd and user fallback"
	case needImageEntrypoint:
		return "entrypoint/cmd fallback"
	case needImageUser:
		return "user fallback"
	default:
		return "runtime fallback"
	}
}
