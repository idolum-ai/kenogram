package plan

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

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

func TestDiffRepeatedFieldsAreInsertionAware(t *testing.T) {
	tests := []struct {
		name   string
		before Plan
		after  Plan
		path   string
	}{
		{
			name:   "workspace paths",
			before: Plan{Workspace: []string{"/a", "/b"}},
			after:  Plan{Workspace: []string{"/x", "/a", "/b"}},
			path:   "workspace_paths[0]",
		},
		{
			name:   "copies",
			before: Plan{Copies: []Copy{copyNamed("a"), copyNamed("b")}},
			after:  Plan{Copies: []Copy{copyNamed("x"), copyNamed("a"), copyNamed("b")}},
			path:   "copies[0]",
		},
		{
			name:   "mounts",
			before: Plan{Mounts: []Mount{{Source: "/a", Target: "/a", Mode: "ro"}, {Source: "/b", Target: "/b", Mode: "ro"}}},
			after:  Plan{Mounts: []Mount{{Source: "/x", Target: "/x", Mode: "ro"}, {Source: "/a", Target: "/a", Mode: "ro"}, {Source: "/b", Target: "/b", Mode: "ro"}}},
			path:   "mounts[0]",
		},
		{
			name:   "network allowances",
			before: Plan{NetworkAllow: []NetworkAllow{{Host: "a.example", Port: 1}, {Host: "b.example", Port: 2}}},
			after:  Plan{NetworkAllow: []NetworkAllow{{Host: "x.example", Port: 3}, {Host: "a.example", Port: 1}, {Host: "b.example", Port: 2}}},
			path:   "network_allow[0]",
		},
		{
			name:   "interfaces",
			before: Plan{Interfaces: []Interface{}},
			after:  Plan{Interfaces: []Interface{{Name: "x", Address: "127.0.0.1:3"}}},
			path:   "interfaces[0]",
		},
		{
			name:   "services",
			before: Plan{Services: []Service{serviceNamed("a"), serviceNamed("b")}},
			after:  Plan{Services: []Service{serviceNamed("x"), serviceNamed("a"), serviceNamed("b")}},
			path:   "services[0]",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changes, err := Diff(test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != 1 || changes[0].Path != test.path || changes[0].Before != "" || changes[0].After == "" {
				t.Fatalf("changes = %#v", changes)
			}
		})
	}
}

func TestDiffSequenceEditShapes(t *testing.T) {
	a, b, c, x := copyNamed("a"), copyNamed("b"), copyNamed("c"), copyNamed("x")
	b2 := b
	b2.Source = "b2"

	tests := []struct {
		name   string
		before []Copy
		after  []Copy
		want   []Change
	}{
		{
			name:   "first insert",
			before: []Copy{},
			after:  []Copy{a},
			want:   []Change{{Path: "copies[0]", After: rendered(t, a)}},
		},
		{
			name:   "last remove",
			before: []Copy{a},
			after:  []Copy{},
			want:   []Change{{Path: "copies[0]", Before: rendered(t, a)}},
		},
		{
			name:   "insert",
			before: []Copy{a, b, c},
			after:  []Copy{x, a, b, c},
			want:   []Change{{Path: "copies[0]", After: rendered(t, x)}},
		},
		{
			name:   "modify",
			before: []Copy{a, b, c},
			after:  []Copy{a, b2, c},
			want:   []Change{{Path: "copies[1].source", Before: `"b"`, After: `"b2"`}},
		},
		{
			name:   "remove",
			before: []Copy{a, b, c},
			after:  []Copy{a, c},
			want:   []Change{{Path: "copies[1]", Before: rendered(t, b)}},
		},
		{
			name:   "reorder",
			before: []Copy{a, b, c},
			after:  []Copy{b, a, c},
			want: []Change{
				{Path: "copies[0]", Before: rendered(t, a)},
				{Path: "copies[1]", After: rendered(t, a)},
			},
		},
		{
			name:   "duplicate occurrence",
			before: []Copy{a, a, b},
			after:  []Copy{a, b},
			want:   []Change{{Path: "copies[1]", Before: rendered(t, a)}},
		},
		{
			name:   "duplicate insertion",
			before: []Copy{a, a, b},
			after:  []Copy{a, x, a, b},
			want:   []Change{{Path: "copies[1]", After: rendered(t, x)}},
		},
		{
			name:   "insert and modify",
			before: []Copy{a, b, c},
			after:  []Copy{x, a, b2, c},
			want: []Change{
				{Path: "copies[0]", After: rendered(t, x)},
				{Path: "copies[2].source", Before: `"b"`, After: `"b2"`},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changes, err := Diff(Plan{Copies: test.before}, Plan{Copies: test.after})
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%#v", changes) != fmt.Sprintf("%#v", test.want) {
				t.Fatalf("changes\n got: %#v\nwant: %#v", changes, test.want)
			}
		})
	}
}

func TestDiffPreservesNilAndEmptyCopies(t *testing.T) {
	changes, err := Diff(Plan{}, Plan{Copies: []Copy{}})
	if err != nil {
		t.Fatal(err)
	}
	want := Change{Path: "copies", Before: "null", After: "[]"}
	if len(changes) != 1 || changes[0] != want {
		t.Fatalf("nil to empty copies = %#v, want %#v", changes, want)
	}
}

func TestDiffSecretCopiesNeverExposeDigestsAcrossSequenceEdits(t *testing.T) {
	secretA := copyNamed("secret-a")
	secretA.Secret = true
	secretA.SourceDigest = "SECRET-DIGEST-A-CANARY"
	secretB := copyNamed("secret-b")
	secretB.Secret = true
	secretB.SourceDigest = "SECRET-DIGEST-B-CANARY"
	changedA := secretA
	changedA.SourceDigest = "SECRET-DIGEST-CHANGED-CANARY"
	plain := copyNamed("plain")

	tests := []struct {
		name   string
		before []Copy
		after  []Copy
	}{
		{name: "insert", before: []Copy{plain}, after: []Copy{secretA, plain}},
		{name: "remove", before: []Copy{secretA, plain}, after: []Copy{plain}},
		{name: "modify", before: []Copy{secretA}, after: []Copy{changedA}},
		{name: "reorder", before: []Copy{secretA, secretB}, after: []Copy{secretB, secretA}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := Plan{Copies: test.before}
			after := Plan{Copies: test.after}
			changes, err := Diff(before, after)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(changes)
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			for _, change := range changes {
				fmt.Fprintf(&text, "change: %s: %s -> %s\n", change.Path, change.Before, change.After)
			}
			planText := fmt.Sprint(changes) + text.String() + fmt.Errorf("plan comparison failed: %v", changes).Error() + string(encoded)
			resultJSON, err := JSON(Result{Plan: after})
			if err != nil {
				t.Fatal(err)
			}
			planText += string(resultJSON)
			for _, canary := range []string{secretA.SourceDigest, secretB.SourceDigest, changedA.SourceDigest} {
				if strings.Contains(planText, canary) {
					t.Fatalf("secret digest %q exposed in %s", canary, planText)
				}
			}
			if len(changes) == 0 {
				t.Fatal("sequence edit produced no evidence")
			}
		})
	}
}

func TestDiffBoundsLargeSequenceAlignment(t *testing.T) {
	before := make([]string, 1024)
	after := make([]string, 1024)
	for i := range before {
		before[i] = fmt.Sprintf("/before/%d", i)
		after[i] = fmt.Sprintf("/after/%d", i)
	}
	changes, err := Diff(Plan{Workspace: before}, Plan{Workspace: after})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "workspace_paths" {
		t.Fatalf("large sequence fallback = %#v", changes)
	}
}

func TestExactSequenceMatchesUsesInclusiveGlobalBudget(t *testing.T) {
	t.Run("exact boundary", func(t *testing.T) {
		d := differ{}
		sequence := make([]any, 1023)
		if _, bounded := d.exactSequenceMatches(sequence, sequence); !bounded {
			t.Fatal("alignment at exact cell budget fell back")
		}
		if d.alignedCells != maxAlignmentCells {
			t.Fatalf("aligned cells = %d, want %d", d.alignedCells, maxAlignmentCells)
		}
		if _, bounded := d.exactSequenceMatches(nil, nil); bounded {
			t.Fatal("alignment beyond exhausted budget did not fall back")
		}
	})

	t.Run("cumulative", func(t *testing.T) {
		d := differ{}
		sequence := make([]any, 511)
		for i := 0; i < 4; i++ {
			if _, bounded := d.exactSequenceMatches(sequence, sequence); !bounded {
				t.Fatalf("alignment %d fell back before cumulative budget", i+1)
			}
		}
		if d.alignedCells != maxAlignmentCells {
			t.Fatalf("aligned cells = %d, want %d", d.alignedCells, maxAlignmentCells)
		}
		if _, bounded := d.exactSequenceMatches(nil, nil); bounded {
			t.Fatal("alignment beyond cumulative budget did not fall back")
		}
	})
}

func TestDiffLargeSequenceFallbackStillReportsSecretDigestChange(t *testing.T) {
	before := make([]Copy, 1024)
	for i := range before {
		before[i] = copyNamed(fmt.Sprintf("secret-%d", i))
		before[i].Secret = true
		before[i].SourceDigest = fmt.Sprintf("SECRET-BEFORE-%d", i)
	}
	after := append([]Copy(nil), before...)
	after[512].SourceDigest = "SECRET-AFTER-CANARY"
	changes, err := Diff(Plan{Copies: before}, Plan{Copies: after})
	if err != nil {
		t.Fatal(err)
	}
	want := Change{Path: "copies[512].source_digest", Before: "<secret>", After: "<secret changed>"}
	if len(changes) != 1 || changes[0] != want {
		t.Fatalf("large secret fallback = %#v", changes)
	}
	if strings.Contains(fmt.Sprint(changes), before[512].SourceDigest) || strings.Contains(fmt.Sprint(changes), after[512].SourceDigest) {
		t.Fatalf("large fallback leaked a secret digest: %#v", changes)
	}
}

func TestDiffLargeSequenceFallbackSummarizesMixedSecretChange(t *testing.T) {
	before := make([]Copy, 1024)
	for i := range before {
		before[i] = copyNamed(fmt.Sprintf("secret-%d", i))
		before[i].Secret = true
		before[i].SourceDigest = fmt.Sprintf("SECRET-BEFORE-%d", i)
	}
	after := append([]Copy(nil), before...)
	after[0].Mode = "0400"
	after[512].SourceDigest = "SECRET-AFTER-CANARY"
	changes, err := Diff(Plan{Copies: before}, Plan{Copies: after})
	if err != nil {
		t.Fatal(err)
	}
	wantSummary := Change{
		Path:   "copies[*].source_digest",
		Before: "<secret digest multiset>",
		After:  "<secret digest multiset changed>",
	}
	if len(changes) != 2 || changes[0].Path != "copies" || changes[1] != wantSummary {
		t.Fatalf("mixed large fallback = %#v", changes)
	}
	encoded := fmt.Sprint(changes)
	for _, canary := range []string{before[512].SourceDigest, after[512].SourceDigest} {
		if strings.Contains(encoded, canary) {
			t.Fatalf("mixed large fallback leaked %q: %#v", canary, changes)
		}
	}
}

func TestLargeSecretDigestSummaryIgnoresReorder(t *testing.T) {
	left := copyNamed("left")
	left.Secret = true
	left.SourceDigest = "left-digest"
	right := copyNamed("right")
	right.Secret = true
	right.SourceDigest = "right-digest"
	d := differ{beforeCopies: []Copy{left, right}, afterCopies: []Copy{right, left}}
	if changes := d.largeSecretDigestSummary("copies"); len(changes) != 0 {
		t.Fatalf("pure reorder reported secret digest change: %#v", changes)
	}
}

func copyNamed(name string) Copy {
	return Copy{Source: name, SourceDigest: "digest", Target: "/" + name, Mode: "0600"}
}

func serviceNamed(name string) Service {
	return Service{Name: name, Command: []string{"/" + name}, Restart: "never"}
}

func rendered(t *testing.T, value any) string {
	t.Helper()
	decoded, err := decodeValue(value)
	if err != nil {
		t.Fatal(err)
	}
	return renderValue(decoded)
}
