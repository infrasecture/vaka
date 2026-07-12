// cmd/vaka/registryclient.go
package main

import (
	"fmt"
	"io"
	"sort"
	"time"

	"vaka.dev/vaka/pkg/registry"
)

// browseIndexMaxAge is the freshness window for catalog commands (search,
// recipes list/info): a younger cache costs no network. vaka get always
// revalidates (maxAge 0).
const browseIndexMaxAge = 15 * time.Minute

// Test seams (same pattern as execDockerComposeFn).
var (
	loadRegistriesConfig = registry.LoadConfig
	registryConfigPath   = registry.ConfigPath
	newRegistryClient    = func(maxAge time.Duration) *registry.Client {
		return &registry.Client{MaxIndexAge: maxAge}
	}
)

// registryWorld is everything the recipe commands need: the configuration
// and each registry's index (where fetchable).
type registryWorld struct {
	cfg     *registry.Config
	client  *registry.Client
	indexes map[string]*registry.Index
}

// loadRegistryWorld loads the config and fetches registry indexes. When only
// is non-empty, just that one registry is fetched (a qualified reference
// needs no others); when empty, every configured registry is fetched (browse
// and unqualified-resolution need all of them). Per-registry fetch failures
// and stale-cache fallbacks become warnings on warnOut; a registry without an
// index is simply absent from the map (unqualified resolution skips it,
// qualified resolution errors).
func loadRegistryWorld(maxAge time.Duration, only string, warnOut io.Writer) (*registryWorld, error) {
	cfg, err := loadRegistriesConfig()
	if err != nil {
		return nil, err
	}
	w := &registryWorld{
		cfg:     cfg,
		client:  newRegistryClient(maxAge),
		indexes: map[string]*registry.Index{},
	}
	for _, reg := range cfg.Registries {
		if only != "" && reg.Name != only {
			continue
		}
		res, err := w.client.FetchIndex(reg)
		if err != nil {
			fmt.Fprintf(warnOut, "vaka: warning: registry %q unavailable: %v\n", reg.Name, err)
			continue
		}
		if res.Stale {
			fmt.Fprintf(warnOut,
				"vaka: warning: registry %q could not be reached; using a cached index that is %s old\n",
				reg.Name, res.Age.Round(time.Minute))
		}
		w.indexes[reg.Name] = res.Index
	}
	return w, nil
}

// catalogRow is one line of the search / recipes list output.
type catalogRow struct {
	registry string
	name     string
	entry    registry.IndexEntry
}

// catalogRows flattens the world into rows (latest version per recipe),
// sorted by registry then name. Qualified display names are used when more
// than one registry is configured.
func (w *registryWorld) catalogRows() []catalogRow {
	var rows []catalogRow
	for regName, idx := range w.indexes {
		for name := range idx.Recipes {
			ref := registry.Ref{Registry: regName, Name: name}
			res, err := registry.Resolve(w.cfg, w.indexes, ref)
			if err != nil {
				continue // no parseable versions
			}
			rows = append(rows, catalogRow{registry: regName, name: name, entry: res.Entry})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].registry != rows[j].registry {
			return rows[i].registry < rows[j].registry
		}
		return rows[i].name < rows[j].name
	})
	return rows
}

// displayName qualifies the recipe name when several registries are
// configured, matching what the user must type to disambiguate.
func (w *registryWorld) displayName(row catalogRow) string {
	if len(w.cfg.Registries) > 1 {
		return row.registry + "/" + row.name
	}
	return row.name
}

func riskColumn(e registry.IndexEntry) string {
	if e.Policy == nil {
		return "?"
	}
	if n := len(e.Policy.RiskFlags); n > 0 {
		return fmt.Sprintf("%d", n)
	}
	return "-"
}
