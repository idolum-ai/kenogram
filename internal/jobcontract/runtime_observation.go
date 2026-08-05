package jobcontract

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var runtimeHexDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var runtimeContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

const RuntimeReadOnlyPermissionPolicy = "portable-readonly-v1"
const RuntimeWorkspacePermissionPolicy = "portable-writable-v1"

func ValidateRuntimeObservation(value RuntimeObservation) error {
	if value.Schema != RuntimeObservationSchema || (value.Phase != "before" && value.Phase != "after") || value.Provider != "podman-cli" {
		return errors.New("runtime observation schema, phase, or provider is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, value.ObservedAt); err != nil {
		return errors.New("runtime observation timestamp is invalid")
	}
	if !runtimeContainerID.MatchString(value.ContainerID) || value.ContainerName == "" || strings.ContainsAny(value.ContainerName, "\x00/\r\n") {
		return errors.New("runtime container identity is invalid")
	}
	if value.ImageReference == "" || !runtimeHexDigest.MatchString(value.ImageDigest) || !runtimeHexDigest.MatchString(value.PlanSHA256) || !runtimeHexDigest.MatchString(value.DeclarationSHA256) || value.Generation < 1 || value.Generation > MaximumWireInteger {
		return errors.New("runtime content identity is invalid")
	}
	if value.NetworkMode == "" || value.IPCMode == "" || value.PIDMode == "" || value.UTSMode == "" || value.UserNSMode == "" || value.User == "" || value.Hostname == "" || !filepath.IsAbs(value.WorkingDirectory) || filepath.Clean(value.WorkingDirectory) != value.WorkingDirectory {
		return errors.New("runtime configuration observation is invalid")
	}
	if value.Devices < 0 || value.MemoryBytes < 1 || value.NanoCPUs < 1 || value.PIDs < 1 {
		return errors.New("runtime resource observation is invalid")
	}
	seen := map[string]struct{}{}
	for _, mount := range value.Mounts {
		validRole := mount.Role == "declared" || mount.Role == "workspace" || mount.Role == "helper" || mount.Role == "lifecycle"
		validRoleType := mount.Role == "declared" || ((mount.Role == "helper" || mount.Role == "lifecycle") && mount.FileType == "file") || (mount.Role == "workspace" && mount.FileType == "directory")
		validContent := (mount.Mode == "ro" && runtimeHexDigest.MatchString(mount.SHA256)) || (mount.Mode == "rw" && mount.SHA256 == "")
		validAuthority := (mount.Role == "declared" && filepath.IsAbs(mount.AuthoritySource) && filepath.Clean(mount.AuthoritySource) == mount.AuthoritySource) || (mount.Role != "declared" && mount.AuthoritySource == "")
		declaredReadOnly := mount.Role == "declared" && mount.Mode == "ro"
		workspace := mount.Role == "workspace"
		validProjection := (declaredReadOnly && runtimeHexDigest.MatchString(mount.AuthoritySHA256) && mount.PermissionPolicy == RuntimeReadOnlyPermissionPolicy) ||
			(workspace && mount.AuthoritySHA256 == "" && mount.PermissionPolicy == RuntimeWorkspacePermissionPolicy) ||
			(!declaredReadOnly && !workspace && mount.AuthoritySHA256 == "" && mount.PermissionPolicy == "")
		expectedSource, sourceErr := RuntimeMountSource(mount.Role, mount.Target, mount.Mode, mount.AuthoritySource, mount.SHA256)
		if !validRole || !validRoleType || !validContent || !validAuthority || !validProjection || sourceErr != nil || mount.Source != expectedSource || !filepath.IsAbs(mount.Target) || filepath.Clean(mount.Target) != mount.Target || (mount.Mode != "ro" && mount.Mode != "rw") || mount.Device > uint64(MaximumWireInteger) || mount.Inode == 0 || mount.Inode > uint64(MaximumWireInteger) || (mount.FileType != "file" && mount.FileType != "directory") || !mount.IdentityVerified {
			return fmt.Errorf("runtime mount %q is invalid", mount.Target)
		}
		if _, duplicate := seen[mount.Target]; duplicate {
			return fmt.Errorf("duplicate runtime mount %q", mount.Target)
		}
		seen[mount.Target] = struct{}{}
	}
	if len(value.Mounts) == 0 || len(value.Mounts) > MaxRuntimeMounts || !sort.SliceIsSorted(value.Mounts, func(i, j int) bool { return value.Mounts[i].Target < value.Mounts[j].Target }) {
		return errors.New("runtime mount inventory is empty or unsorted")
	}
	if value.Phase == "after" && value.EgressAdmission != nil {
		return errors.New("stopped runtime observation cannot claim a live egress admission")
	}
	if value.EgressAdmission != nil {
		admission := value.EgressAdmission
		host, portText, splitErr := net.SplitHostPort(admission.ListenerAddress)
		port, portErr := strconv.Atoi(portText)
		if !validDigest(admission.AllowlistSHA256) || !ownerIDPattern.MatchString(admission.OwnerID) ||
			admission.PID < 1 || admission.PID > MaximumWireInteger || !validOpaqueText(admission.ProcessStart, 1, 256) ||
			!validNamespaceIdentity(admission.UserNamespace) || !validNamespaceIdentity(admission.NetworkNamespace) ||
			splitErr != nil || portErr != nil || host != "127.0.0.1" || port < 1 || port > 65535 || net.JoinHostPort(host, strconv.Itoa(port)) != admission.ListenerAddress {
			return errors.New("runtime egress admission is invalid")
		}
	}
	return nil
}
