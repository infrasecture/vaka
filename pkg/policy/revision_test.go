package policy_test

import (
	"testing"

	"vaka.dev/vaka/pkg/policy"
)

func TestRevisionTracksSemanticsButNotGeneratorVersion(t *testing.T) {
	base := &policy.ServicePolicy{
		APIVersion:             "agent.vaka/v1alpha1",
		Kind:                   "ServicePolicy",
		GeneratedBy:            "vaka/v0.1.0",
		RequiredRuntimeVersion: "v0.1.0",
		Services: map[string]*policy.ServiceConfig{
			"app": {Runtime: &policy.RuntimeConfig{DropCaps: []string{"NET_ADMIN"}}},
		},
	}

	first, err := policy.Revision(base)
	if err != nil {
		t.Fatal(err)
	}
	base.GeneratedBy = "vaka/v9.0.0"
	second, err := policy.Revision(base)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("generator-only change altered revision: %q != %q", first, second)
	}

	base.RequiredRuntimeVersion = "v0.2.0"
	third, err := policy.Revision(base)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("runtime compatibility change did not alter revision")
	}
}

func TestRevisionIsStableAcrossMapInsertionOrder(t *testing.T) {
	one := &policy.ServicePolicy{
		APIVersion: "agent.vaka/v1alpha1",
		Kind:       "ServicePolicy",
		Services: map[string]*policy.ServiceConfig{
			"a": {},
			"b": {},
		},
	}
	two := &policy.ServicePolicy{
		APIVersion: "agent.vaka/v1alpha1",
		Kind:       "ServicePolicy",
		Services: map[string]*policy.ServiceConfig{
			"b": {},
			"a": {},
		},
	}
	oneRevision, err := policy.Revision(one)
	if err != nil {
		t.Fatal(err)
	}
	twoRevision, err := policy.Revision(two)
	if err != nil {
		t.Fatal(err)
	}
	if oneRevision != twoRevision {
		t.Fatalf("map insertion order changed revision: %q != %q", oneRevision, twoRevision)
	}
}
