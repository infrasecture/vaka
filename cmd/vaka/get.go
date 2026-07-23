// cmd/vaka/get.go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/spf13/cobra"
	"vaka.dev/vaka/pkg/recipe"
	"vaka.dev/vaka/pkg/registry"
)

func newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <[registry/]name>[@version] [dir]",
		Short: "Fetch or update a recipe from a registry",
		Long: `Fetch a recipe into a new directory, or update a previously fetched one.

The reference is [registry/]name[@version]; the version must be exact
(constraints are not supported) and defaults to the newest published one.
The target directory defaults to ./<name>. vaka get never runs the recipe
and never adopts an existing non-recipe directory; updates never touch
files you created or modified.`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: firstArgComplete(completeRecipeRefs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGet(cmd, args)
		},
	}
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	ref, err := registry.ParseRef(args[0])
	if err != nil {
		return err
	}
	// A qualified reference needs only its own registry's index; an
	// unqualified one needs all of them for the uniqueness rule. get always
	// revalidates (maxAge 0) and is strict (an unreachable, uncached registry
	// is fatal, not a warning) on this security-sensitive path.
	world, err := loadRegistryWorld(0, ref.Registry, true, errOut)
	if err != nil {
		return err
	}
	res, err := registry.Resolve(world.cfg, world.indexes, ref)
	if err != nil {
		return err
	}

	// Enforce the recipe's minimum vaka version before doing any work: a
	// recipe that needs a newer vaka must not be installed by this one.
	if err := checkMinVakaVersion(res.Entry.MinVakaVersion, version, errOut); err != nil {
		return err
	}

	target := "./" + res.Name
	if len(args) == 2 {
		target = args[1]
	}

	var (
		verb     string
		lock     *recipe.Lock
		warnings []string
	)
	// Fast path: a directory already at exactly this recipe and digest and
	// fully pristine needs no download or update.
	upToDate, err := recipe.UpToDate(target, res.Registry.Name, res.Name, res.Entry.Version, res.Entry.Digest)
	if err != nil {
		return err
	}
	if upToDate {
		verb = "already up to date in"
	} else {
		tarball, err := world.client.FetchTarball(res.Registry, res.Name, res.Entry)
		if err != nil {
			return err
		}
		defer os.Remove(tarball)
		verb, lock, warnings, err = installOrUpdate(res, tarball, target)
		if err != nil {
			return err
		}
	}
	for _, w := range warnings {
		fmt.Fprintf(errOut, "vaka: warning: %s\n", w)
	}

	fmt.Fprintf(out, "%s@%s %s %s (digest %s)\n", res.Name, res.Entry.Version, verb, target, res.Entry.Digest)
	printDeviations(out, lock)

	// The local lint is authoritative; the index block is only compared.
	summary, lintErr := recipe.LintDir(context.Background(), target)
	if lintErr != nil {
		fmt.Fprintf(errOut, "vaka: warning: policy summary unavailable: %v\n", lintErr)
	} else {
		var idxActions map[string]string
		var idxFlags []string
		if res.Entry.Policy != nil {
			idxActions, idxFlags = res.Entry.Policy.DefaultActions, res.Entry.Policy.RiskFlags
		}
		for _, w := range recipe.CompareWithIndex(summary, idxActions, idxFlags,
			fmt.Sprintf("%s@%s", res.Name, res.Entry.Version)) {
			fmt.Fprintf(errOut, "vaka: warning: %s\n", w)
		}
		printPolicySummary(out, summary)
	}

	printUnsetRequiredEnv(out, res.Entry.Env)
	if res.Entry.MinVakaVersion != "" {
		fmt.Fprintf(out, "Requires vaka >= %s\n", res.Entry.MinVakaVersion)
	}
	fmt.Fprintf(out, "Next: cd %s && vaka up\n", target)
	return nil
}

// checkMinVakaVersion enforces a recipe's minVakaVersion against the running
// vaka. A recipe needing a newer vaka is refused (hard block). Builds whose
// self-version is not a SemVer (e.g. "dev") skip the check with a warning,
// since there is nothing to compare.
func checkMinVakaVersion(minVersion, vakaVersion string, warnOut io.Writer) error {
	if minVersion == "" {
		return nil
	}
	min, err := semver.NewVersion(minVersion)
	if err != nil {
		fmt.Fprintf(warnOut, "vaka: warning: recipe declares an unparseable minVakaVersion %q; not enforced\n", minVersion)
		return nil
	}
	cur, err := semver.NewVersion(vakaVersion)
	if err != nil {
		fmt.Fprintf(warnOut, "vaka: warning: this build's version %q is not a release version; cannot enforce minVakaVersion %s\n", vakaVersion, minVersion)
		return nil
	}
	if cur.LessThan(min) {
		return fmt.Errorf("recipe requires vaka >= %s, but this is %s; upgrade vaka", minVersion, vakaVersion)
	}
	return nil
}

// installOrUpdate routes to the fresh-install or update engine based on the
// target's lock, never adopting foreign directories.
func installOrUpdate(res *registry.Resolved, tarball, target string) (verb string, lock *recipe.Lock, warnings []string, err error) {
	if _, statErr := os.Lstat(target); statErr != nil {
		if !os.IsNotExist(statErr) {
			return "", nil, nil, statErr
		}
		lock, err = recipe.Install(recipe.InstallSpec{
			Registry: res.Registry.Name, Name: res.Name,
			Version: res.Entry.Version, Digest: res.Entry.Digest,
			TarballPath: tarball, Target: target, VakaVersion: version,
		})
		return "installed into", lock, nil, err
	}

	// Route install vs update by lock presence, and refuse to adopt a
	// foreign (non-recipe) directory. The recipe-identity guard (right
	// recipe/registry) is enforced authoritatively inside recipe.Update,
	// under its update lock.
	_, hasLock, err := recipe.LockForDir(target)
	if err != nil {
		return "", nil, nil, err
	}
	if !hasLock {
		return "", nil, nil, fmt.Errorf(
			"target %q already exists and is not a vaka-managed recipe directory; vaka get never adopts or writes into an existing path",
			target)
	}

	updRes, err := recipe.Update(recipe.UpdateSpec{
		Registry: res.Registry.Name, Name: res.Name,
		Version: res.Entry.Version, Digest: res.Entry.Digest,
		TarballPath: tarball, Target: target, VakaVersion: version,
	})
	if err != nil {
		return "", nil, nil, err
	}
	verb = "updated in"
	if updRes.NoChange {
		verb = "already up to date in"
	}
	return verb, updRes.Lock, updRes.Warnings, nil
}

func printDeviations(out io.Writer, lock *recipe.Lock) {
	if lock == nil || len(lock.Deviations) == 0 {
		return
	}
	fmt.Fprintf(out, "Deviations from the published recipe (%d):\n", len(lock.Deviations))
	for _, d := range lock.Deviations {
		fmt.Fprintf(out, "  %s (%s)\n", d.Path, d.Kind)
	}
}

func printPolicySummary(out io.Writer, s *recipe.LocalPolicySummary) {
	services := make([]string, 0, len(s.DefaultActions))
	for svc := range s.DefaultActions {
		services = append(services, svc)
	}
	sort.Strings(services)
	parts := make([]string, len(services))
	for i, svc := range services {
		parts[i] = svc + ": " + s.DefaultActions[svc]
	}
	fmt.Fprintf(out, "Egress policy: %s\n", strings.Join(parts, ", "))
	if len(s.RiskFlags) > 0 {
		fmt.Fprintf(out, "RISK FLAGS: %s\n", strings.Join(s.RiskFlags, ", "))
	}
}

func printUnsetRequiredEnv(out io.Writer, env []registry.EnvVar) {
	var missing []string
	for _, e := range env {
		// Only truly-absent counts as unset; an explicitly empty value is set.
		if _, ok := os.LookupEnv(e.Name); e.Required && !ok {
			missing = append(missing, e.Name)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(out, "Required env not set: %s\n", strings.Join(missing, ", "))
	}
}

// printDeviationNotice is the render-verb hook (design §6): a one-line
// notice while an instantiated recipe deviates from its published version.
// The lock is read through a confinement root with a bounded read
// (recipe.LockForDir), since the project directory is untrusted content.
func printDeviationNotice(w io.Writer, projectDir string) {
	if projectDir == "" {
		projectDir = "."
	}
	lock, exists, err := recipe.LockForDir(projectDir)
	if err != nil || !exists || len(lock.Deviations) == 0 {
		return
	}
	fmt.Fprintf(w, "vaka: note: this directory deviates from published %s@%s in %d file(s); run 'vaka get %s' after resolving to converge\n",
		lock.Name, lock.Version, len(lock.Deviations), lock.Name)
}
