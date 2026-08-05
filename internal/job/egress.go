package job

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/idolum-ai/kenogram/internal/jobcontract"
	"github.com/idolum-ai/kenogram/internal/plan"
)

var EgressEnvironmentKeys = []string{"ALL_PROXY", "HTTPS_PROXY", "HTTP_PROXY", "all_proxy", "https_proxy", "http_proxy"}

func EgressAllowlistDigest(allows []plan.NetworkAllow) string {
	values := make([]string, 0, len(allows))
	for _, allow := range allows {
		host := strings.ToLower(strings.TrimSuffix(allow.Host, "."))
		values = append(values, net.JoinHostPort(host, strconv.FormatInt(allow.Port, 10)))
	}
	sort.Strings(values)
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func verifyEgressEvidence(observation jobcontract.EgressEvidence, retained plan.Result, result jobcontract.Result, runtimeBefore jobcontract.RuntimeObservation) error {
	if len(retained.Plan.NetworkAllow) == 0 {
		return fmt.Errorf("networkless job retained egress evidence")
	}
	if observation.AllowlistSHA256 != EgressAllowlistDigest(retained.Plan.NetworkAllow) {
		return fmt.Errorf("egress allowlist digest disagrees with retained authority")
	}
	if observation.ContainerID != runtimeBefore.ContainerID || observation.Generation != result.Identity.Generation || observation.Generation != runtimeBefore.Generation {
		return fmt.Errorf("egress runtime identity disagrees with container evidence")
	}
	admission := runtimeBefore.EgressAdmission
	if admission == nil {
		return fmt.Errorf("egress admission is absent from independently retained runtime evidence")
	}
	if observation.AllowlistSHA256 != admission.AllowlistSHA256 || observation.ListenerAddress != admission.ListenerAddress || observation.OwnerID != admission.OwnerID ||
		observation.PID != admission.PID || observation.ProcessStart != admission.ProcessStart || observation.UserNamespace != admission.UserNamespace || observation.NetworkNamespace != admission.NetworkNamespace {
		return fmt.Errorf("egress lifecycle identity disagrees with independently retained runtime admission")
	}
	if result.Status == "complete" && observation.Status != "complete" {
		return fmt.Errorf("complete result lacks complete egress lifecycle evidence")
	}
	if observation.Status == "complete" && (!result.Cleanup.ProxyAbsent || runtimeBefore.NetworkMode != "none") {
		return fmt.Errorf("complete egress evidence lacks proxy absence or network-none containment")
	}
	if result.Target.Kind == "exited" || result.Target.Kind == "signaled" {
		ready, readyErr := time.Parse(time.RFC3339Nano, observation.ReadyAt)
		started, startedErr := time.Parse(time.RFC3339Nano, result.Target.StartedAt)
		finished, finishedErr := time.Parse(time.RFC3339Nano, result.Target.FinishedAt)
		revoked, revokedErr := time.Parse(time.RFC3339Nano, observation.RevokedAt)
		finalized, finalizedErr := time.Parse(time.RFC3339Nano, result.Finalization.FinishedAt)
		if err := errors.Join(readyErr, startedErr, finishedErr, revokedErr, finalizedErr); err != nil {
			return fmt.Errorf("egress lifecycle timestamps are invalid: %w", err)
		}
		if ready.After(started) || revoked.Before(finished) || revoked.After(finalized) {
			return fmt.Errorf("egress lifecycle is outside target and finalization boundaries")
		}
	}
	return nil
}
