package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/idolum-ai/kenogram/internal/app"
	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/doctor"
	"github.com/idolum-ai/kenogram/internal/naming"
	"github.com/idolum-ai/kenogram/internal/netns"
	"github.com/idolum-ai/kenogram/internal/plan"
	"github.com/idolum-ai/kenogram/internal/proxy"
	"github.com/idolum-ai/kenogram/internal/version"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stderr)
		return 2
	}
	if args[0] == backend.AppleMachineBridgeCommand {
		if runtime.GOOS != "linux" {
			fmt.Fprintln(stderr, "runtime: the Apple machine bridge is Linux-only")
			return 1
		}
		decoded, err := backend.DecodeAppleMachineArguments(args[1:])
		if err != nil {
			fmt.Fprintln(stderr, "runtime: decode Apple machine arguments:", err)
			return 1
		}
		return run(decoded, stdout, stderr)
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		fmt.Fprintln(stdout, version.String())
		return 0
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printHelp(stdout)
		return 0
	}
	ctx, stop := operationContext(context.Background())
	defer stop()
	launcher, err := backend.AppleMachineFromEnvironment(runtime.GOOS, os.Getenv, nil)
	if err != nil {
		fmt.Fprintln(stderr, "runtime:", err)
		return 1
	}
	if launcher != nil {
		if err := launcher.Launch(ctx, args); err != nil {
			return reportLauncherError(err, stderr)
		}
		return 0
	}
	switch args[0] {
	case "_netns-listener":
		return runNetnsListener(args[1:], stderr)
	case "_netns-connect":
		return runNetnsConnect(args[1:], stderr)
	case "_proxy":
		return runProxy(args[1:], stderr)
	case "up":
		return runUp(ctx, args[1:], stdout, stderr)
	case "down":
		return runWorldAction(ctx, "down", args[1:], stdout, stderr)
	case "destroy":
		return runWorldAction(ctx, "destroy", args[1:], stdout, stderr)
	case "enter":
		return runEnter(ctx, args[1:], stdout, stderr)
	case "connect":
		return runConnect(ctx, args[1:], stdout, stderr)
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr)
	case "allow":
		return runAllow(args[1:], stdout, stderr)
	case "revoke":
		return runRevoke(args[1:], stdout, stderr)
	case "repair-history":
		return runRepairHistory(args[1:], stdout, stderr)
	case "worlds":
		return runWorlds(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "unknown command:", args[0])
		printHelp(stderr)
		return 2
	}
}

func operationContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case received := <-signals:
			cancel(&backend.SignalCause{Signal: received})
		case <-done:
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			signal.Stop(signals)
			close(done)
			cancel(context.Canceled)
		})
	}
}

func reportLauncherError(err error, stderr io.Writer) int {
	var remote *backend.RemoteExitError
	if errors.As(err, &remote) {
		return remote.Code
	}
	fmt.Fprintln(stderr, "runtime:", err)
	return 1
}

var newApp = func(stdout io.Writer) (*app.App, error) {
	a, err := app.New()
	if err != nil {
		return nil, err
	}
	a.Out = stdout
	return a, nil
}

var inspectHost = doctor.Inspect

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: kenogram doctor [--json]"
	if helpRequested(args) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	stateDir, err := worldfs.BaseDir()
	if err != nil {
		fmt.Fprintln(stderr, "doctor:", err)
		return 1
	}
	report := inspectHost(ctx, stateDir)
	if *jsonOut {
		if code := encode(stdout, stderr, report); code != 0 {
			return code
		}
	} else {
		for _, check := range report.Checks {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", strings.ToUpper(check.Status), check.Name, terminalField(check.Observed))
			if check.Status == "fail" && check.Remediation != "" {
				fmt.Fprintf(stdout, "  remedy: %s\n", terminalField(check.Remediation))
			}
		}
		fmt.Fprintf(stdout, "ready: %t\n", report.Ready)
	}
	if !report.Ready {
		return 1
	}
	return 0
}

func terminalField(value string) string {
	var clean strings.Builder
	for _, r := range value {
		switch r {
		case '\n':
			clean.WriteString(`\n`)
		case '\r':
			clean.WriteString(`\r`)
		case '\t':
			clean.WriteString(`\t`)
		case '\x1b':
			clean.WriteString(`\u001b`)
		default:
			if r < ' ' || r == '\x7f' {
				fmt.Fprintf(&clean, `\u%04x`, r)
			} else {
				clean.WriteRune(r)
			}
		}
	}
	return clean.String()
}

func runUp(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: kenogram up [--dry-run] [--json] [--yes] <file>"
	if helpRequested(args) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }
	dry := fs.Bool("dry-run", false, "stop after plan")
	jsonOut := fs.Bool("json", false, "JSON output")
	yes := fs.Bool("yes", false, "confirm replacement")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	prepared, err := app.Prepare(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	a, err := newApp(stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := a.ValidateHostMounts(prepared.Result); err != nil {
		fmt.Fprintln(stderr, "validate host mounts:", err)
		return 1
	}
	comparison, err := a.CompareUpContext(ctx, prepared)
	if err != nil {
		fmt.Fprintln(stderr, "compare prior world:", err)
		return 1
	}
	planWriter := stdout
	if *jsonOut {
		planWriter = stderr
	}
	if !*jsonOut || !*dry {
		if err := plan.RenderText(planWriter, prepared.Result); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		for _, change := range comparison.Changes {
			fmt.Fprintf(planWriter, "change: %s: %s -> %s\n", change.Path, change.Before, change.After)
		}
		if comparison.Workspace != "" {
			fmt.Fprintln(planWriter, comparison.Workspace)
		}
	}
	if *dry {
		if *jsonOut {
			return encode(stdout, stderr, struct {
				Result    plan.Result   `json:"result"`
				Changes   []plan.Change `json:"changes"`
				Workspace string        `json:"workspace"`
			}{prepared.Result, comparison.Changes, comparison.Workspace})
		}
		return 0
	}
	if !*yes && !confirm(stderr) {
		fmt.Fprintln(stderr, "refusing to change a world without --yes (review the plan above)")
		return 2
	}
	if *jsonOut {
		a.Out = stderr
	}
	if err := a.UpReviewed(ctx, prepared, comparison); err != nil {
		fmt.Fprintln(stderr, "up:", err)
		return 1
	}
	if *jsonOut {
		return encode(stdout, stderr, map[string]any{"outcome": "applied", "world": prepared.Result.Plan.Name, "plan_digest": prepared.Result.PlanDigest, "declaration_digest": prepared.Result.DeclarationDigest})
	}
	return 0
}
func confirm(w io.Writer) bool {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	fmt.Fprint(w, "Apply this plan? [y/N] ")
	var answer string
	fmt.Fscan(os.Stdin, &answer)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes")
}
func runWorldAction(ctx context.Context, action string, args []string, stdout, stderr io.Writer) int {
	usage := fmt.Sprintf("usage: kenogram %s <world>", action)
	if action == "destroy" {
		usage = "usage: kenogram destroy --yes <world>"
	}
	if helpRequested(args) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }
	yes := false
	if action == "destroy" {
		fs.BoolVar(&yes, "yes", false, "confirm destructive action")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	if err := worldfs.ValidateName(name); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if action == "destroy" && !yes {
		fmt.Fprintln(stderr, "destroy requires --yes")
		return 2
	}
	a, err := newApp(stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if action == "down" {
		err = a.Down(ctx, name)
	} else {
		err = a.Destroy(ctx, name)
	}
	if err != nil {
		fmt.Fprintln(stderr, action+":", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: %s\n", name, action)
	return 0
}
func runEnter(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: kenogram enter [--repair] <world>"
	if helpRequested(args) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	fs := flag.NewFlagSet("enter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }
	repair := fs.Bool("repair", false, "open a bare shell")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if err := worldfs.ValidateName(fs.Arg(0)); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	a, err := newApp(io.Discard)
	if err == nil {
		err = a.Enter(ctx, fs.Arg(0), *repair)
	}
	if err != nil {
		fmt.Fprintln(stderr, "enter:", err)
		return 1
	}
	return 0
}

func runConnect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: kenogram connect <world> <interface>"
	if helpRequested(args) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if len(args) != 2 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if err := worldfs.ValidateName(args[0]); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := naming.Interface(args[1]); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	a, err := newApp(io.Discard)
	if err != nil {
		fmt.Fprintln(stderr, "connect:", err)
		return 1
	}
	connection, err := a.Connect(ctx, args[0], args[1])
	if err == nil {
		err = relayConnection(ctx, os.Stdin, stdout, connection)
	}
	if err != nil {
		fmt.Fprintln(stderr, "connect:", err)
		return 1
	}
	return 0
}

func relayConnection(ctx context.Context, input io.Reader, output io.Writer, connection net.Conn) error {
	defer connection.Close()
	type directionResult struct {
		direction string
		err       error
	}
	results := make(chan directionResult, 2)
	go func() {
		_, err := io.Copy(connection, input)
		if tcp, ok := connection.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		results <- directionResult{direction: "upload", err: err}
	}()
	go func() {
		_, err := io.Copy(output, connection)
		results <- directionResult{direction: "download", err: err}
	}()
	abort := func() {
		_ = connection.Close()
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	for completed := 0; completed < 2; completed++ {
		select {
		case <-ctx.Done():
			abort()
			return context.Cause(ctx)
		case result := <-results:
			if result.err != nil {
				abort()
				return result.err
			}
			// A server may finish its output while it continues accepting input.
			// Preserve that legal TCP half-close until the upload also completes.
			if result.direction == "download" {
				if tcp, ok := connection.(*net.TCPConn); ok {
					_ = tcp.CloseRead()
				}
			}
		}
	}
	return nil
}
func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: kenogram status [--json] <world>"
	if helpRequested(args) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if err := worldfs.ValidateName(fs.Arg(0)); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	a, err := newApp(io.Discard)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	status, err := a.Status(ctx, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "status:", err)
		return 1
	}
	payload := newStatusPayload(status)
	if *jsonOut {
		return encode(stdout, stderr, payload)
	}
	world := fs.Arg(0)
	stateLabel := "unknown"
	if status.Authoritative != nil && status.Authoritative.State.Status != "" {
		stateLabel = status.Authoritative.State.Status
	}
	if status.RecoveryPhase != "" {
		stateLabel = "recovery-required:" + status.RecoveryPhase
	}
	fmt.Fprintf(stdout, "world: %s\nstatus: %s\n", world, stateLabel)
	printGeneration := func(label string, observation *app.GenerationObservation) {
		if observation == nil {
			fmt.Fprintf(stdout, "%s: none\n", label)
			return
		}
		running, network := false, ""
		if observation.Evidence != nil {
			running, network = observation.Evidence.Running, observation.Evidence.NetworkMode
		}
		fmt.Fprintf(stdout, "%s generation: g%d\n%s container: %s\n%s plan digest: %s\n%s declaration digest: %s\n%s runtime exists: %t\n%s runtime running: %t\n%s network mode: %s\n", label, observation.State.Generation, label, observation.State.Container, label, observation.State.PlanDigest, label, observation.State.DeclarationDigest, label, observation.Exists, label, running, label, network)
	}
	printGeneration("authoritative", status.Authoritative)
	if status.Candidate != nil {
		printGeneration("candidate", status.Candidate)
	}
	return 0
}

type statusPayload struct {
	// State and RuntimeEvidence preserve the pre-transition-aware JSON fields.
	State           any               `json:"state,omitempty"`
	RuntimeEvidence any               `json:"runtime_evidence,omitempty"`
	RuntimeExists   bool              `json:"runtime_exists"`
	Status          app.StatusResult  `json:"status"`
	Sources         map[string]string `json:"sources"`
}

func newStatusPayload(status app.StatusResult) statusPayload {
	sources := map[string]string{"declared": "applied.toml", "recorded": "state.json", "observed": "podman inspect"}
	if status.RecoveryPhase != "" {
		sources["declared"] = "transition.json"
		sources["recorded"] = "transition.json"
	}
	payload := statusPayload{Status: status, Sources: sources}
	if status.Authoritative != nil {
		payload.State = status.Authoritative.State
		payload.RuntimeExists = status.Authoritative.Exists
		payload.RuntimeEvidence = status.Authoritative.Evidence
	}
	return payload
}
func runWorlds(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: kenogram worlds [--json]"
	if helpRequested(args) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	fs := flag.NewFlagSet("worlds", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	a, err := newApp(io.Discard)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	states, err := a.Worlds()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *jsonOut {
		return encode(stdout, stderr, states)
	}
	for _, s := range states {
		fmt.Fprintf(stdout, "%s\tg%d\t%s\t%s\n", s.Name, s.Generation, s.Status, s.PlanDigest)
	}
	return 0
}
func runAllow(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: kenogram allow <world> <host>:<port> --for <duration>"
	if helpRequested(args) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if len(args) != 4 || args[2] != "--for" || args[3] == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if err := worldfs.ValidateName(args[0]); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	a, err := newApp(io.Discard)
	if err == nil {
		err = a.Allow(args[0], args[1], args[3])
	}
	if err != nil {
		fmt.Fprintln(stderr, "allow:", err)
		return 1
	}
	fmt.Fprintf(stdout, "granted %s to %s for %s\n", args[1], args[0], args[3])
	return 0
}
func runRevoke(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: kenogram revoke <world> <host>:<port>"
	if helpRequested(args) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if len(args) != 2 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if err := worldfs.ValidateName(args[0]); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	a, err := newApp(io.Discard)
	if err == nil {
		err = a.Revoke(args[0], args[1])
	}
	if err != nil {
		fmt.Fprintln(stderr, "revoke:", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s from %s\n", args[1], args[0])
	return 0
}
func runRepairHistory(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: kenogram repair-history --yes <world>"
	if helpRequested(args) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if len(args) != 2 || args[0] != "--yes" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if err := worldfs.ValidateName(args[1]); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	a, err := newApp(io.Discard)
	if err == nil {
		err = a.RepairHistory(args[1])
	}
	if err != nil {
		fmt.Fprintln(stderr, "repair-history:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: truncated history tail removed\n", args[1])
	return 0
}

func helpRequested(args []string) bool {
	return len(args) == 1 && (args[0] == "--help" || args[0] == "-h")
}
func encode(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

type repeated []string

func (r *repeated) String() string         { return strings.Join(*r, ",") }
func (r *repeated) Set(value string) error { *r = append(*r, value); return nil }
func runProxy(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("_proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pid := fs.Int("pid", 0, "world pid")
	control := fs.String("control", "", "control socket")
	logPath := fs.String("log", "", "metadata log")
	var allows repeated
	fs.Var(&allows, "allow", "allowed host:port")
	if err := fs.Parse(args); err != nil || *pid <= 0 || *control == "" || *logPath == "" {
		return 2
	}
	destinations := []proxy.Destination{}
	for _, raw := range allows {
		d, err := proxy.ParseDestination(raw)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		destinations = append(destinations, d)
	}
	file, err := os.OpenFile(*logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer file.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := netns.AcquireListener(ctx, *pid, "127.0.0.1:3128")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer listener.Close()
	p := proxy.New(destinations, proxy.Options{Logger: log.New(file, "", log.LstdFlags|log.LUTC)})
	go func() { <-ctx.Done(); listener.Close() }()
	go func() {
		if err := p.ServeControl(*control); err != nil {
			fmt.Fprintln(stderr, err)
			stop()
		}
	}()
	if err := p.Serve(listener); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
func runNetnsListener(args []string, stderr io.Writer) int {
	fd, address, err := netns.ParseHelperArgs(args)
	if err == nil {
		err = netns.SendListener(fd, address)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
func runNetnsConnect(args []string, stderr io.Writer) int {
	fd, address, err := netns.ParseHelperArgs(args)
	if err == nil {
		err = netns.SendConnection(fd, address)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
func printHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  kenogram up [--dry-run] [--json] [--yes] <file>
  kenogram down <world>
  kenogram destroy --yes <world>
  kenogram enter [--repair] <world>
  kenogram connect <world> <interface>
  kenogram status [--json] <world>
  kenogram allow <world> <host>:<port> --for <duration>
  kenogram revoke <world> <host>:<port>
  kenogram repair-history --yes <world>
  kenogram worlds [--json]
  kenogram doctor [--json]
  kenogram version
  kenogram help
`)
}
