package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	doctorLookPath    = exec.LookPath
	doctorDockerProbe = runDoctorDockerCommand
)

type doctorCheck struct {
	name        string
	remediation string
	run         func(context.Context) (string, error)
}

type doctorResult struct {
	name    string
	ok      bool
	detail  string
	errText string
	fix     string
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight checks for Docker/Compose compatibility",
		RunE: func(cmd *cobra.Command, args []string) error {
			results := runDoctorChecks(context.Background(), defaultDoctorChecks())
			failed := 0
			for _, r := range results {
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
}

func runDoctorChecks(ctx context.Context, checks []doctorCheck) []doctorResult {
	results := make([]doctorResult, 0, len(checks))
	for _, c := range checks {
		detail, err := c.run(ctx)
		if err != nil {
			results = append(results, doctorResult{
				name:    c.name,
				ok:      false,
				errText: strings.TrimSpace(err.Error()),
				fix:     c.remediation,
			})
			continue
		}
		results = append(results, doctorResult{
			name:   c.name,
			ok:     true,
			detail: strings.TrimSpace(detail),
		})
	}
	return results
}

func defaultDoctorChecks() []doctorCheck {
	return []doctorCheck{
		{
			name:        "docker CLI available",
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
			remediation: "Start Docker and verify your current Docker context/daemon is reachable.",
			run: func(ctx context.Context) (string, error) {
				stdout, stderr, err := doctorDockerProbe(ctx, []string{"version", "--format", "{{.Server.Version}}"})
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
			remediation: "Install/enable Docker Compose v2 (`docker compose`).",
			run: func(ctx context.Context) (string, error) {
				stdout, stderr, err := doctorDockerProbe(ctx, []string{"compose", "version", "--short"})
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
			remediation: "Use a Docker backend that runs Linux containers (Docker Desktop Linux containers mode or Linux Engine).",
			run: func(ctx context.Context) (string, error) {
				stdout, stderr, err := doctorDockerProbe(ctx, []string{"info", "--format", "{{.OSType}}"})
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
			name:        "active docker context",
			remediation: "Set a valid context (`docker context use <name>`) or pass `--context` to vaka commands.",
			run: func(ctx context.Context) (string, error) {
				stdout, stderr, err := doctorDockerProbe(ctx, []string{"context", "show"})
				if err != nil {
					return "", fmt.Errorf("%s", firstNonEmpty(stderr, stdout, err.Error()))
				}
				name := strings.TrimSpace(stdout)
				if name == "" {
					return "", fmt.Errorf("active context name is empty")
				}
				return name, nil
			},
		},
	}
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
