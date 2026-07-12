package plan

import "testing"

func TestDiffReportsExactPaths(t *testing.T) {
	before := Plan{Name: "x", Resources: Resources{CPUs: 1}, NetworkAllow: []NetworkAllow{{Host: "a", Port: 443}}}
	after := before
	after.Resources.CPUs = 2
	after.NetworkAllow = []NetworkAllow{{Host: "b", Port: 443}}
	changes, err := Diff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, change := range changes {
		paths[change.Path] = true
	}
	for _, want := range []string{"resources.cpus", "network_allow[0].host"} {
		if !paths[want] {
			t.Fatalf("missing %s: %#v", want, changes)
		}
	}
}

func TestDiffRedactsWithoutMutatingPlans(t *testing.T) {
	before := Plan{Copies: []Copy{{SourceDigest: "before-secret-digest", Secret: true}}}
	after := Plan{Copies: []Copy{{SourceDigest: "after-secret-digest", Secret: true}}}
	changes, err := Diff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if before.Copies[0].SourceDigest != "before-secret-digest" || after.Copies[0].SourceDigest != "after-secret-digest" {
		t.Fatalf("Diff mutated inputs: before=%q after=%q", before.Copies[0].SourceDigest, after.Copies[0].SourceDigest)
	}
	if len(changes) != 1 || changes[0] != (Change{Path: "copies[0].source_digest", Before: "<secret>", After: "<secret changed>"}) {
		t.Fatalf("secret changes = %#v", changes)
	}
}
