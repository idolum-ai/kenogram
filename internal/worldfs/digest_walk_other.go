//go:build !linux

package worldfs

import (
	"context"
	"fmt"
	"runtime"
)

func walkDigestRoot(context.Context, string, DigestLimits) ([]DigestEntry, error) {
	return nil, fmt.Errorf("workspace digest is available only in the Linux execution environment, not %s", runtime.GOOS)
}
