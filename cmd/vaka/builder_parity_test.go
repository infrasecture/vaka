package main

import "testing"

func TestRunFullAndShowComposeUseSameOverrideBuilder(t *testing.T) {
	tests := []struct {
		name       string
		runFullArg []string
		showArg    []string
		composeYML string
		runtimes   map[string]ResolvedRuntime
	}{
		{
			name:       "without_build_flag",
			runFullArg: []string{"up"},
			showArg:    []string{"show-compose"},
			composeYML: `
services:
  app:
    image: alpine:3.20
    user: "1000:1000"
    entrypoint: ["sleep"]
    command: ["infinity"]
`,
		},
		{
			name:       "with_build_flag",
			runFullArg: []string{"up", "--build"},
			showArg:    []string{"show-compose", "--build"},
			composeYML: `
services:
  app:
    image: app:latest
    build: .
`,
			runtimes: map[string]ResolvedRuntime{
				"app": {Entrypoint: []string{"/bin/app"}, Command: []string{"serve"}, ImageUser: "1000:1000"},
			},
		},
	}

	policyYAML := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			chdirForTest(t, dir)
			writeFixtureFiles(t, dir, policyYAML, tc.composeYML)

			ds := &fakeBuilderDockerServices{runtimes: tc.runtimes}
			setDockerServicesFactoryForTest(t, ds)

			var runFullYAML string
			setExecDockerComposeForTest(t, func(args []string, overrideYAML string, extraEnv []string) error {
				if overrideYAML != "" {
					runFullYAML = overrideYAML
				}
				return nil
			})

			if err := runFull("vaka.yaml", tc.runFullArg, true); err != nil {
				t.Fatalf("runFull: %v", err)
			}
			if runFullYAML == "" {
				t.Fatal("runFull did not produce override YAML")
			}

			showComposeYAML, err := captureStdout(t, func() error {
				return runShowCompose("vaka.yaml", tc.showArg, true)
			})
			if err != nil {
				t.Fatalf("runShowCompose: %v", err)
			}

			if showComposeYAML != runFullYAML {
				t.Fatalf("override mismatch\n--- runFull ---\n%s\n--- show-compose ---\n%s", runFullYAML, showComposeYAML)
			}
		})
	}
}
