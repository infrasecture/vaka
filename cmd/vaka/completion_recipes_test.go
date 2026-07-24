// cmd/vaka/completion_recipes_test.go
package main

import (
	"strings"
	"testing"
)

func TestCompleteRecipeRefs(t *testing.T) {
	// fixtureRegistry configures a single registry "testreg" and writes its
	// index cache; a single registry means unqualified names.
	fixtureRegistry(t, matchingPolicyBlock())

	// Prime the cache: completion reads cached indexes only.
	if _, _, err := runRecipeCmd(t, "recipes", "list"); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	got, _ := completeRecipeRefs("")
	if len(got) != 1 || got[0] != "demo" {
		t.Fatalf("completions = %v, want [demo]", got)
	}
	if got, _ := completeRecipeRefs("de"); len(got) != 1 || got[0] != "demo" {
		t.Fatalf("prefix completions = %v, want [demo]", got)
	}
	if got, _ := completeRecipeRefs("zz"); len(got) != 0 {
		t.Fatalf("non-matching prefix = %v, want none", got)
	}
}

func TestCompleteRegistryNames(t *testing.T) {
	fixtureRegistry(t, matchingPolicyBlock()) // testreg

	got, _ := completeRegistryNames("")
	if len(got) != 1 || got[0] != "testreg" {
		t.Fatalf("registry completions = %v, want [testreg]", got)
	}
	if got, _ := completeRegistryNames("test"); len(got) != 1 {
		t.Fatalf("prefix = %v", got)
	}
	if got, _ := completeRegistryNames("other"); len(got) != 0 {
		t.Fatalf("non-matching = %v", got)
	}
}

func TestGetCompletionViaComplete(t *testing.T) {
	fixtureRegistry(t, matchingPolicyBlock())
	if _, _, err := runRecipeCmd(t, "recipes", "list"); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	// The __complete machinery drives ValidArgsFunction end to end.
	stdout, _, err := runRecipeCmd(t, "__complete", "get", "")
	if err != nil {
		t.Fatalf("__complete get: %v", err)
	}
	if !strings.Contains(stdout, "demo") {
		t.Fatalf("get completion did not offer demo:\n%s", stdout)
	}
}
