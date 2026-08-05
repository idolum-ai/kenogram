package integration

import (
	"strings"
	"testing"
)

func TestCanonicalPodmanImageID(t *testing.T) {
	hex := strings.Repeat("a", 64)
	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "bare", value: hex, want: "sha256:" + hex},
		{name: "prefixed", value: "sha256:" + hex, want: "sha256:" + hex},
		{name: "short", value: hex[:63]},
		{name: "long", value: hex + "a"},
		{name: "nonhex", value: strings.Repeat("g", 64)},
		{name: "uppercase", value: strings.Repeat("A", 64)},
		{name: "wrong algorithm", value: "sha512:" + hex},
		{name: "leading whitespace", value: " " + hex},
		{name: "trailing whitespace", value: hex + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := canonicalPodmanImageID(test.value)
			if test.want == "" {
				if err == nil {
					t.Fatalf("canonicalPodmanImageID(%q)=%q, want error", test.value, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("canonicalPodmanImageID(%q)=%q, %v; want %q", test.value, got, err, test.want)
			}
		})
	}
}
