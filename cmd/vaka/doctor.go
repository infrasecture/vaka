package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

var (
	doctorLookPath          = exec.LookPath
	doctorDockerProbe       = runDoctorDockerCommand
	newDoctorDockerServices = NewDockerServices
)

const (
	doctorProbeTimeout      = 10 * time.Second
	doctorDefaultFixTimeout = 5 * time.Minute
)

type doctorOptions struct {
	fix bool
}

type doctorCheck struct {
	name        string
	required    bool
	timeout     time.Duration
	fixTimeout  time.Duration
	dependsOn   []string
	remediation string
	run         func(context.Context) (string, error)
	fix         func(context.Context) (string, error)
}

type doctorResult struct {
	name     string
	required bool
	ok       bool
	skipped  bool
	skipText string
	detail   string
	errText  string
	fix      string

	fixAttempted bool
	fixApplied   bool
	fixDetail    string
	fixErrText   string
	postFixErr   string
}

func newDoctorCmd() *cobra.Command {
	opts := doctorOptions{}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight checks for Docker/Compose compatibility",
		RunE: func(cmd *cobra.Command, args []string) error {
			results := runDoctorChecks(context.Background(), defaultDoctorChecks(), opts.fix)
			failed := 0
			for _, r := range results {
				if r.skipped {
					if r.required {
						fmt.Printf("SKIP %s: %s\n", r.name, r.skipText)
					} else {
						fmt.Printf("INFO %s: skipped (%s)\n", r.name, r.skipText)
					}
					continue
				}

				if !r.required {
					if r.ok {
						detail := doctorResultDetail(r)
						if strings.TrimSpace(detail) == "" {
							fmt.Printf("INFO %s\n", r.name)
						} else {
							fmt.Printf("INFO %s: %s\n", r.name, detail)
						}
					} else {
						fmt.Printf("INFO %s: unavailable (%s)\n", r.name, r.errText)
						if strings.TrimSpace(r.fixErrText) != "" {
							fmt.Printf("  fix attempt failed: %s\n", r.fixErrText)
						}
					}
					continue
				}

				if r.ok {
					detail := doctorResultDetail(r)
					if strings.TrimSpace(detail) == "" {
						fmt.Printf("PASS %s\n", r.name)
					} else {
						fmt.Printf("PASS %s: %s\n", r.name, detail)
					}
					continue
				}
				failed++
				fmt.Printf("FAIL %s: %s\n", r.name, r.errText)
				if r.fixApplied {
					if strings.TrimSpace(r.fixDetail) == "" {
						fmt.Printf("  fix applied\n")
					} else {
						fmt.Printf("  fix applied: %s\n", r.fixDetail)
					}
				}
				if strings.TrimSpace(r.postFixErr) != "" {
					fmt.Printf("  fix applied but check still failing: %s\n", r.postFixErr)
				}
				if strings.TrimSpace(r.fixErrText) != "" {
					fmt.Printf("  fix attempt failed: %s\n", r.fixErrText)
				}
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
	cmd.Flags().BoolVar(&opts.fix, "fix", false, "Attempt available auto-fixes for failing checks, then re-run those checks.")
	return cmd
}

func runDoctorChecks(ctx context.Context, checks []doctorCheck, applyFix bool) []doctorResult {
	results := make([]doctorResult, 0, len(checks))
	byName := make(map[string]doctorResult, len(checks))
	for _, c := range checks {
		if dep, depErr, failed := failedDependency(c.dependsOn, byName); failed {
			skip := doctorResult{
				name:     c.name,
				required: c.required,
				skipped:  true,
				fix:      c.remediation,
				skipText: fmt.Sprintf("prerequisite %q failed", dep),
			}
			if strings.TrimSpace(depErr) != "" {
				skip.skipText = fmt.Sprintf("prerequisite %q failed: %s", dep, depErr)
			}
			results = append(results, skip)
			byName[c.name] = skip
			continue
		}

		detail, err := runDoctorStep(ctx, c.timeout, c.run)
		r := doctorResult{
			name:     c.name,
			required: c.required,
			fix:      c.remediation,
		}
		if err == nil {
			r.ok = true
			r.detail = strings.TrimSpace(detail)
			results = append(results, r)
			continue
		}
		r.errText = strings.TrimSpace(err.Error())

		if applyFix && c.fix != nil {
			r.fixAttempted = true
			fixTimeout := resolveDoctorFixTimeout(c)
			fixDetail, fixErr := runDoctorStep(ctx, fixTimeout, c.fix)
			if fixErr != nil {
				r.fixErrText = strings.TrimSpace(fixErr.Error())
				byName[c.name] = r
				results = append(results, r)
				continue
			}
			r.fixApplied = true
			r.fixDetail = strings.TrimSpace(fixDetail)

			// Re-run the probe after a successful fix to ensure the check now passes.
			detail, err = runDoctorStep(ctx, c.timeout, c.run)
			if err != nil {
				r.postFixErr = strings.TrimSpace(err.Error())
				byName[c.name] = r
				results = append(results, r)
				continue
			}
			r.ok = true
			r.detail = strings.TrimSpace(detail)
		}

		results = append(results, r)
		byName[c.name] = r
	}
	return results
}

func resolveDoctorFixTimeout(c doctorCheck) time.Duration {
	if c.fixTimeout > 0 {
		return c.fixTimeout
	}
	if doctorDefaultFixTimeout > 0 {
		return doctorDefaultFixTimeout
	}
	return c.timeout
}

func failedDependency(dependsOn []string, byName map[string]doctorResult) (name string, errText string, failed bool) {
	for _, dep := range dependsOn {
		r, ok := byName[dep]
		if !ok {
			continue
		}
		if !r.ok {
			return dep, strings.TrimSpace(r.errText), true
		}
	}
	return "", "", false
}

func runDoctorStep(ctx context.Context, timeout time.Duration, run func(context.Context) (string, error)) (string, error) {
	stepCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		stepCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	detail, err := run(stepCtx)
	cancel()
	if timeout > 0 && errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", timeout.Round(time.Second))
	}
	return detail, err
}

func doctorResultDetail(r doctorResult) string {
	detail := strings.TrimSpace(r.detail)
	if !r.fixApplied {
		return detail
	}
	fixInfo := "fixed"
	if strings.TrimSpace(r.fixDetail) != "" {
		fixInfo = "fixed: " + strings.TrimSpace(r.fixDetail)
	}
	if detail == "" {
		return fixInfo
	}
	return detail + " (" + fixInfo + ")"
}

func defaultDoctorChecks() []doctorCheck {
	vakaInitImageRef := vakaInitBaseImage + ":" + version
	isDevBuild := strings.TrimSpace(version) == "dev"

	var (
		dsOnce sync.Once
		ds     DockerServices
		dsErr  error
	)
	getDockerServices := func() (DockerServices, error) {
		dsOnce.Do(func() {
			ds, dsErr = newDoctorDockerServices(nil)
		})
		if dsErr != nil {
			return nil, dsErr
		}
		return ds, nil
	}

	imageRemediation := fmt.Sprintf(
		"If the image is missing, `vaka doctor --fix` can pull it. Otherwise verify Docker target reachability/auth, or pull manually: `docker pull %s`.",
		vakaInitImageRef,
	)
	var imageFix func(context.Context) (string, error)
	if isDevBuild {
		imageRemediation = fmt.Sprintf(
			"Not auto-fixable on unstamped dev builds (version=%q). Build with a stamped VERSION (tag/SHA) so the required helper image tag resolves.",
			version,
		)
	} else {
		imageFix = func(ctx context.Context) (string, error) {
			ds, err := getDockerServices()
			if err != nil {
				return "", err
			}
			if err := ds.EnsureImage(ctx, vakaInitImageRef); err != nil {
				return "", err
			}
			return "pulled " + vakaInitImageRef, nil
		}
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
			required:    true,
			timeout:     doctorProbeTimeout,
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
			required:    true,
			timeout:     doctorProbeTimeout,
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
			name:        "required vaka-init image present",
			required:    true,
			timeout:     doctorProbeTimeout,
			fixTimeout:  doctorDefaultFixTimeout,
			dependsOn:   []string{"docker daemon reachable"},
			remediation: imageRemediation,
			run: func(ctx context.Context) (string, error) {
				if isDevBuild {
					return "", fmt.Errorf(
						"unstamped dev build (version=%q) resolves helper image to %s, which is not published (not auto-fixable)",
						version, vakaInitImageRef,
					)
				}
				ds, err := getDockerServices()
				if err != nil {
					return "", err
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
			fix: imageFix,
		},
		{
			name:     "resolved docker context",
			required: false,
			timeout:  doctorProbeTimeout,
			run: func(ctx context.Context) (string, error) {
				stdout, stderr, err := doctorDockerProbe(ctx, []string{"context", "show"})
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
