package main

import "testing"

func TestRunFullAndShowComposeUseSameOverrideBuilder(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)

	policyYAML := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`
	composeYAML := `
services:
  app:
    image: alpine:3.20
    user: "1000:1000"
    entrypoint: ["sleep"]
    command: ["infinity"]
`
	writeFixtureFiles(t, dir, policyYAML, composeYAML)

	ds := &fakeBuilderDockerServices{}
	setDockerServicesFactoryForTest(t, ds)

	var runFullYAML string
	setExecDockerComposeForTest(t, func(args []string, overrideYAML string, extraEnv []string) error {
		if overrideYAML != "" {
			runFullYAML = overrideYAML
		}
		return nil
	})

	if err := runFull("vaka.yaml", []string{"up"}, true); err != nil {
		t.Fatalf("runFull: %v", err)
	}
	if runFullYAML == "" {
		t.Fatal("runFull did not produce override YAML")
	}

	showComposeYAML, err := captureStdout(t, func() error {
		return runShowCompose("vaka.yaml", []string{"show-compose"}, true)
	})
	if err != nil {
		t.Fatalf("runShowCompose: %v", err)
	}

	if showComposeYAML != runFullYAML {
		t.Fatalf("override mismatch\n--- runFull ---\n%s\n--- show-compose ---\n%s", runFullYAML, showComposeYAML)
	}
}
