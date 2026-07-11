package plan

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// RenderText writes a deterministic, secret-content-free plan summary.
func RenderText(w io.Writer, result Result) error {
	p := result.Plan
	if _, err := fmt.Fprintf(w, "Kenogram plan\nworld: %s\nplan digest: %s\ndeclaration digest: %s\nbase: %s\nworkdir: %s\nuser: %s\nresources: cpus=%d memory_bytes=%d pids=%d\n", p.Name, result.PlanDigest, result.DeclarationDigest, p.World.Base, p.World.Workdir, p.World.User, p.Resources.CPUs, p.Resources.MemoryBytes, p.Resources.PIDs); err != nil {
		return err
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintln(w, "warning:", warning); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "workspace:", strings.Join(p.Workspace, ", ")); err != nil {
		return err
	}
	for _, c := range p.Copies {
		if _, err := fmt.Fprintf(w, "copy: %s -> %s mode=%s secret=%t\n", c.Source, c.Target, c.Mode, c.Secret); err != nil {
			return err
		}
	}
	for _, m := range p.Mounts {
		if _, err := fmt.Fprintf(w, "mount: %s -> %s mode=%s\n", m.Source, m.Target, m.Mode); err != nil {
			return err
		}
	}
	for _, a := range p.NetworkAllow {
		if _, err := fmt.Fprintf(w, "network: %s:%d\n", a.Host, a.Port); err != nil {
			return err
		}
	}
	for _, s := range p.Services {
		if _, err := fmt.Fprintf(w, "service: %s command=%s autostart=%t restart=%s\n", s.Name, quoteCommand(s.Command), s.Autostart, s.Restart); err != nil {
			return err
		}
	}
	return nil
}

func quoteCommand(command []string) string {
	quoted := make([]string, len(command))
	for i, arg := range command {
		quoted[i] = strconv.Quote(arg)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
