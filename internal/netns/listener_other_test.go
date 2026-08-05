//go:build !linux

package netns

import (
	"context"
	"strings"
	"testing"
)

func TestNamespaceOperationsFailClosedOutsideLinux(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "acquire listener", run: func() error {
			_, err := AcquireListener(context.Background(), 1, "127.0.0.1:1")
			return err
		}},
		{name: "send listener", run: func() error { return SendListener(3, "/tmp/control") }},
		{name: "acquire connection", run: func() error {
			_, err := AcquireConnection(context.Background(), 1, "start", "127.0.0.1:1", func() error { return nil })
			return err
		}},
		{name: "send connection", run: func() error { return SendConnection(3, "/tmp/control") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil || !strings.Contains(err.Error(), "require Linux") {
				t.Fatalf("error = %v, want explicit Linux refusal", err)
			}
		})
	}
}

func TestNamespaceHelperArgumentsFailClosedOutsideLinux(t *testing.T) {
	t.Parallel()
	if _, _, err := ParseHelperArgs([]string{"--control-fd", "3", "--address", "127.0.0.1:1"}); err == nil ||
		!strings.Contains(err.Error(), "require Linux") {
		t.Fatalf("error = %v, want explicit Linux refusal", err)
	}
}
