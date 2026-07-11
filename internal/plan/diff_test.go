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
