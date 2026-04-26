package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	doctorLookPath          = exec.LookPath
	doctorDockerProbe       = runDoctorDockerCommand
	newDoctorDockerServices = NewDockerServices
)

const (
	doctorProbeTimeout = 10 * time.Second
	doctorFixTimeout   = 5 * time.Minute
)

type doctorOptions struct {
	fix     bool
	context string
}

type doctorCheck struct {
	name        string
	required    bool
	timeout     time.Duration
	remediation string
	run         func(context.Context) (string, error)
}

type doctorResult struct {
	name     string
	required bool
	ok       bool
	detail   string
	errText  string
	fix      string
}

func newDoctorCmd() *cobra.Command {
	opts := doctorOptions{}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight checks for Docker/Compose compatibility",
		RunE: func(cmd *cobra.Command, args []string) error {
			results := runDoctorChecks(context.Background(), defaultDoctorChecks(opts))
			failed := 0
			for _, r := range results {
				if !r.required {
					if r.ok {
						if strings.TrimSpace(r.detail) == "" {
							fmt.Printf("INFO %s\n", r.name)
						} else {
							fmt.Printf("INFO %s: %s\n", r.name, r.detail)
						}
					} else {
						fmt.Printf("INFO %s: unavailable (%s)\n", r.name, r.errText)
					}
					continue
				}

				if r.ok {
					if strings.TrimSpace(r.detail) == "" {
						fmt.Printf("PASS %s\n", r.name)
					} else {
						fmt.Printf("PASS %s: %s\n", r.name, r.detail)
					}
					continue
				}
				failed++
				fmt.Printf("FAIL %s: %s\n", r.name, r.errText)
				if strings.TrimSpace(r.fix) != "" {
					fmt.Printf("  fix: %s\n", r.fix)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d preflight check(s) failed", failed)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.fix, "fix", false, "Pull missing required vaka-init image into the selected Docker target.")
	cmd.Flags().StringVarP(&opts.context, "context", "c", "", "Docker context for checks and --fix pull (same as docker --context).")
	return cmd
}

func runDoctorChecks(ctx context.Context, checks []doctorCheck) []doctorResult {
	results := make([]doctorResult, 0, len(checks))
	for _, c := range checks {
		checkCtx := ctx
		cancel := func() {}
		if c.timeout > 0 {
			checkCtx, cancel = context.WithTimeout(ctx, c.timeout)
		}
		detail, err := c.run(checkCtx)
		cancel()

		if c.timeout > 0 && errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s", c.timeout.Round(time.Second))
		}

		if err != nil {
			results = append(results, doctorResult{
				name:     c.name,
				required: c.required,
				ok:       false,
				errText:  strings.TrimSpace(err.Error()),
				fix:      c.remediation,
			})
			continue
		}
		results = append(results, doctorResult{
			name:     c.name,
			required: c.required,
			ok:       true,
			detail:   strings.TrimSpace(detail),
		})
	}
	return results
}

func defaultDoctorChecks(opts doctorOptions) []doctorCheck {
	dockerTargetArgs := doctorTargetArgs(opts.context)
	probeDocker := func(ctx context.Context, args ...string) (string, string, error) {
		full := make([]string, 0, len(dockerTargetArgs)+len(args))
		full = append(full, dockerTargetArgs...)
		full = append(full, args...)
		return doctorDockerProbe(ctx, full)
	}
	vakaInitImageRef := vakaInitBaseImage + ":" + version
	imageTimeout := doctorProbeTimeout
	if opts.fix {
		imageTimeout = doctorFixTimeout
	}
	imageFixHint := fmt.Sprintf("Run `vaka doctor --fix` to pull it, or `docker pull %s`.", vakaInitImageRef)
	if ctxName := strings.TrimSpace(opts.context); ctxName != "" {
		imageFixHint = fmt.Sprintf(
			"Run `vaka doctor --context %s --fix` to pull it, or `docker --context %s pull %s`.",
			ctxName, ctxName, vakaInitImageRef,
		)
	}

	return []doctorCheck{
		{
			name:        "docker CLI available",
			required:    true,
			remediation: "Install Docker Engine or Docker Desktop and ensure `docker` is on PATH.",
			run: func(context.Context) (string, error) {
				p, err := doctorLookPath("docker")
				if err != nil {
					return "", err
				}
				return p, nil
			},
		},
		{
			name:        "docker daemon reachable",
			required:    true,
			timeout:     doctorProbeTimeout,
			remediation: "Start Docker and verify your current Docker context/daemon is reachable.",
			run: func(ctx context.Context) (string, error) {
				stdout, stderr, err := probeDocker(ctx, "version", "--format", "{{.Server.Version}}")
				if err != nil {
					return "", fmt.Errorf("%s", firstNonEmpty(stderr, stdout, err.Error()))
				}
				if strings.TrimSpace(stdout) == "" {
					return "", fmt.Errorf("docker server version output is empty")
				}
				return "server " + strings.TrimSpace(stdout), nil
			},
		},
		{
			name:        "docker compose v2 available",
			required:    true,
			timeout:     doctorProbeTimeout,
			remediation: "Install/enable Docker Compose v2 (`docker compose`).",
			run: func(ctx context.Context) (string, error) {
				stdout, stderr, err := probeDocker(ctx, "compose", "version", "--short")
				if err != nil {
					return "", fmt.Errorf("%s", firstNonEmpty(stderr, stdout, err.Error()))
				}
				v := strings.TrimSpace(stdout)
				major, err := parseComposeMajorVersion(v)
				if err != nil {
					return "", err
				}
				if major < 2 {
					return "", fmt.Errorf("docker compose version %q is unsupported; need v2+", v)
				}
				return v, nil
			},
		},
		{
			name:        "linux container backend",
			required:    true,
			timeout:     doctorProbeTimeout,
			remediation: "Use a Docker backend that runs Linux containers (Docker Desktop Linux containers mode or Linux Engine).",
			run: func(ctx context.Context) (string, error) {
				stdout, stderr, err := probeDocker(ctx, "info", "--format", "{{.OSType}}")
				if err != nil {
					return "", fmt.Errorf("%s", firstNonEmpty(stderr, stdout, err.Error()))
				}
				osType := strings.ToLower(strings.TrimSpace(stdout))
				if osType != "linux" {
					return "", fmt.Errorf("docker daemon reports OSType=%q", osType)
				}
				return "OSType=linux", nil
			},
		},
		{
			name:        "required vaka-init image present",
			required:    true,
			timeout:     imageTimeout,
			remediation: imageFixHint,
			run: func(ctx context.Context) (string, error) {
				ds, err := newDoctorDockerServices(dockerTargetArgs)
				if err != nil {
					return "", err
				}
				if opts.fix {
					if err := ds.EnsureImage(ctx, vakaInitImageRef); err != nil {
						return "", err
					}
					return vakaInitImageRef, nil
				}
				ok, err := ds.ImageExists(ctx, vakaInitImageRef)
				if err != nil {
					return "", err
				}
				if !ok {
					return "", fmt.Errorf("%s is missing in the selected Docker target", vakaInitImageRef)
				}
				return vakaInitImageRef, nil
			},
		},
		{
			name:     "resolved docker context",
			required: false,
			timeout:  doctorProbeTimeout,
			run: func(ctx context.Context) (string, error) {
				stdout, stderr, err := probeDocker(ctx, "context", "show")
				if err != nil {
					return "", fmt.Errorf("%s", firstNonEmpty(stderr, stdout, err.Error()))
				}
				name := strings.TrimSpace(stdout)
				if name == "" {
					return "", fmt.Errorf("resolved context name is empty")
				}
				return name, nil
			},
		},
	}
}

func doctorTargetArgs(contextName string) []string {
	ctx := strings.TrimSpace(contextName)
	if ctx == "" {
		return nil
	}
	return []string{"--context", ctx}
}

func runDoctorDockerCommand(ctx context.Context, args []string) (stdout string, stderr string, err error) {
	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	c := exec.CommandContext(ctx, "docker", args...)
	c.Stdout = &outBuf
	c.Stderr = &errBuf
	err = c.Run()
	return strings.TrimSpace(outBuf.String()), strings.TrimSpace(errBuf.String()), err
}

func parseComposeMajorVersion(version string) (int, error) {
	v := strings.TrimSpace(version)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return 0, fmt.Errorf("docker compose version output is empty")
	}
	majorPart := strings.SplitN(v, ".", 2)[0]
	major, err := strconv.Atoi(majorPart)
	if err != nil {
		return 0, fmt.Errorf("cannot parse docker compose version %q", version)
	}
	return major, nil
}

func firstNonEmpty(parts ...string) string {
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			return strings.TrimSpace(p)
		}
	}
	return ""
}
