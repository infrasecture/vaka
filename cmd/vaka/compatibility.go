package main

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

const (
	minimumDockerEngineVersion = "28.0.0"
	minimumDockerAPIVersion    = "1.48.0"
	minimumComposeVersion      = "2.35.0"
)

func requireMinimumVersion(component, raw, minimum string) (*semver.Version, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, fmt.Errorf("%s version output is empty", component)
	}
	current, err := semver.NewVersion(value)
	if err != nil {
		return nil, fmt.Errorf("cannot parse %s version %q: %w", component, raw, err)
	}
	required, err := semver.StrictNewVersion(minimum)
	if err != nil {
		return nil, fmt.Errorf("invalid built-in minimum %s version %q: %w", component, minimum, err)
	}
	if current.LessThan(required) {
		return nil, fmt.Errorf("%s version %s is unsupported; need %s or newer", component, current, required)
	}
	return current, nil
}

func checkDockerCompatibility(engineVersion, apiVersion string) error {
	if _, err := requireMinimumVersion("Docker Engine", engineVersion, minimumDockerEngineVersion); err != nil {
		return err
	}
	if _, err := requireMinimumVersion("Docker API", apiVersion, minimumDockerAPIVersion); err != nil {
		return err
	}
	return nil
}

func checkComposeCompatibility(composeVersion string) error {
	_, err := requireMinimumVersion("Docker Compose", composeVersion, minimumComposeVersion)
	return err
}
