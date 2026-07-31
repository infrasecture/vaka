package main

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

const (
	minimumDockerEngineVersion        = "28.0.0"
	minimumDockerAPIVersion           = "1.48.0"
	minimumComposeVersion             = "2.35.0"
	imageMountPathBugEngineStart      = "29.0.0"
	imageMountPathBugEngineFix        = "29.2.0"
	composeExpandsImageMountIDVersion = "5.1.0"
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

func checkDockerClientCompatibility(apiVersion string) error {
	_, err := requireMinimumVersion("Docker client API", apiVersion, minimumDockerAPIVersion)
	return err
}

func checkComposeCompatibility(composeVersion string) error {
	_, err := requireMinimumVersion("Docker Compose", composeVersion, minimumComposeVersion)
	return err
}

// checkImageMountVersionCompatibility rejects the one known incompatible
// Engine/Compose pairing. Engine 29.0 and 29.1 hex-encode the complete image
// mount identity into a filesystem name. Compose 5.1+ expands compact image ID
// prefixes to the full sha256 ID, making that name exceed NAME_MAX. Engine 28
// does not use the affected encoding and Engine 29.2+ hashes the identity.
func resolveImageMountVersionCompatibility(engineVersion, composeVersion string) (useCompactSource bool, err error) {
	engine, err := requireMinimumVersion("Docker Engine", engineVersion, minimumDockerEngineVersion)
	if err != nil {
		return false, err
	}
	compose, err := requireMinimumVersion("Docker Compose", composeVersion, minimumComposeVersion)
	if err != nil {
		return false, err
	}

	bugStart := semver.MustParse(imageMountPathBugEngineStart)
	bugFix := semver.MustParse(imageMountPathBugEngineFix)
	expandsID := semver.MustParse(composeExpandsImageMountIDVersion)
	affectedEngine := !engine.LessThan(bugStart) && engine.LessThan(bugFix)
	if affectedEngine && !compose.LessThan(expandsID) {
		return false, fmt.Errorf(
			"Docker Engine %s with Docker Compose %s has an image-mount path-length incompatibility; upgrade Docker Engine to %s or newer (preferred), or use Docker Compose %s through 5.0.x",
			engine, compose, bugFix, semver.MustParse(minimumComposeVersion),
		)
	}
	return affectedEngine, nil
}

func checkImageMountVersionCompatibility(engineVersion, composeVersion string) error {
	_, err := resolveImageMountVersionCompatibility(engineVersion, composeVersion)
	return err
}
