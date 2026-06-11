package agentcli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// newFlagSet builds a flag set that writes its own errors to errw (so a bad flag
// surfaces on the CLI's error stream) and never calls os.Exit.
func newFlagSet(name string, errw io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.Usage = func() {}
	return fs
}

// cmdRun implements `agentrun run <command> [--pool P] [--timeout N]`: create a
// sandbox, run the command, terminate the sandbox, and return the command's exit
// code. Terminate runs even when exec fails.
func cmdRun(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	fs := newFlagSet("run", errw)
	pool := fs.String("pool", "", "pool to create the sandbox from")
	timeout := fs.Int("timeout", 0, "exec timeout in seconds (0 = backend default)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(errw, usage)
		return 2
	}
	command := strings.Join(fs.Args(), " ")
	if command == "" {
		fmt.Fprintf(errw, "run: a command is required\n\n%s", usage)
		return 2
	}

	id, err := backend.Create(ctx, *pool)
	if err != nil {
		fmt.Fprintf(errw, "create sandbox: %v\n", err)
		return 1
	}

	result, execErr := backend.Exec(ctx, id, command, *timeout)

	// Always attempt termination, even on exec error, so a sandbox is not
	// leaked. A terminate failure is reported but does not mask the exec
	// outcome.
	if termErr := backend.Terminate(ctx, id); termErr != nil {
		fmt.Fprintf(errw, "terminate sandbox %s: %v\n", id, termErr)
	}

	if execErr != nil {
		fmt.Fprintf(errw, "exec: %v\n", execErr)
		return 1
	}
	if result.Stdout != "" {
		fmt.Fprint(out, result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(errw, result.Stderr)
	}
	return result.ExitCode
}

// cmdSandbox dispatches the `sandbox` subcommands.
func cmdSandbox(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(errw, "sandbox: a subcommand is required\n\n%s", usage)
		return 2
	}
	switch args[0] {
	case "create":
		return cmdSandboxCreate(ctx, args[1:], backend, out, errw)
	case "ls":
		return cmdSandboxLs(ctx, args[1:], backend, out, errw)
	case "exec":
		return cmdSandboxExec(ctx, args[1:], backend, out, errw)
	case "fork":
		return cmdSandboxFork(ctx, args[1:], backend, out, errw)
	case "terminate":
		return cmdSandboxTerminate(ctx, args[1:], backend, out, errw)
	default:
		fmt.Fprintf(errw, "unknown sandbox subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}

func cmdSandboxCreate(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	fs := newFlagSet("sandbox create", errw)
	pool := fs.String("pool", "", "pool to create the sandbox from")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(errw, usage)
		return 2
	}
	id, err := backend.Create(ctx, *pool)
	if err != nil {
		fmt.Fprintf(errw, "create sandbox: %v\n", err)
		return 1
	}
	fmt.Fprintln(out, id)
	return 0
}

func cmdSandboxLs(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	fs := newFlagSet("sandbox ls", errw)
	namespace := fs.String("n", "", "namespace")
	allNamespaces := fs.Bool("A", false, "all namespaces")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(errw, usage)
		return 2
	}
	ns := *namespace
	if *allNamespaces {
		ns = ""
	}
	infos, err := backend.List(ctx, ns)
	if err != nil {
		fmt.Fprintf(errw, "list sandboxes: %v\n", err)
		return 1
	}
	fmt.Fprint(out, formatSandboxInfos(infos))
	return 0
}

func cmdSandboxExec(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintf(errw, "sandbox exec: <id> and a command are required\n\n%s", usage)
		return 2
	}
	id := args[0]
	command := strings.Join(args[1:], " ")
	result, err := backend.Exec(ctx, id, command, 0)
	if err != nil {
		fmt.Fprintf(errw, "exec: %v\n", err)
		return 1
	}
	if result.Stdout != "" {
		fmt.Fprint(out, result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(errw, result.Stderr)
	}
	return result.ExitCode
}

func cmdSandboxFork(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	// Accept the sandbox id either before or after the flags, so both
	// `fork sbx-1 --replicas 3` and `fork --replicas 3 sbx-1` work. The stdlib
	// flag parser stops at the first non-flag token, so split the id out first.
	id, rest := splitFirstPositional(args)
	fs := newFlagSet("sandbox fork", errw)
	replicas := fs.Int("replicas", 1, "number of forks")
	if err := fs.Parse(rest); err != nil {
		fmt.Fprint(errw, usage)
		return 2
	}
	if id == "" && fs.NArg() > 0 {
		id = fs.Arg(0)
	}
	if id == "" {
		fmt.Fprintf(errw, "sandbox fork: a sandbox id is required\n\n%s", usage)
		return 2
	}
	ids, err := backend.Fork(ctx, id, *replicas)
	if err != nil {
		fmt.Fprintf(errw, "fork: %v\n", err)
		return 1
	}
	for _, fid := range ids {
		fmt.Fprintln(out, fid)
	}
	return 0
}

func cmdSandboxTerminate(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintf(errw, "sandbox terminate: a sandbox id is required\n\n%s", usage)
		return 2
	}
	id := args[0]
	if err := backend.Terminate(ctx, id); err != nil {
		fmt.Fprintf(errw, "terminate: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "terminated %s\n", id)
	return 0
}

// cmdDev validates the `dev up|down` arguments. The dev orchestration shells out
// to kind and kubectl, which the pure CLI dispatcher does not do; cmd/agentrun
// intercepts the dev subcommand before agentcli.Run and runs DevUp/DevDown with
// a real exec runner. Reaching cmdDev means dev was invoked through a path that
// did not wire the runner, so it reports that and returns nonzero.
func cmdDev(_ context.Context, args []string, _, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(errw, "dev: 'up' or 'down' is required\n\n%s", usage)
		return 2
	}
	switch args[0] {
	case "up", "down":
		fmt.Fprintf(errw, "dev %s: run via the agentrun binary, which wires the kind/kubectl runner\n", args[0])
		return 1
	default:
		fmt.Fprintf(errw, "unknown dev subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}

// splitFirstPositional returns the first argument that is not a flag (does not
// start with "-") and the remaining args with that token removed, so a leading
// positional id can appear before flags that the stdlib flag parser would
// otherwise stop at. If there is no positional token, id is empty and rest is
// args unchanged.
func splitFirstPositional(args []string) (id string, rest []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest = make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return a, rest
		}
	}
	return "", args
}

// formatSandboxInfos renders SandboxInfo rows as an aligned table with columns
// NAME POOL PHASE NODE ENDPOINT AGE. An empty list returns a friendly message.
// The age formatting matches the kubectl-style rendering used by the
// kubectl-sandbox plugin (single largest unit).
func formatSandboxInfos(infos []SandboxInfo) string {
	if len(infos) == 0 {
		return "No sandboxes found.\n"
	}
	header := []string{"NAME", "POOL", "PHASE", "NODE", "ENDPOINT", "AGE"}
	rows := make([][]string, 0, len(infos))
	for i := range infos {
		in := &infos[i]
		rows = append(rows, []string{
			in.Name,
			orDash(in.Pool),
			orDash(in.Phase),
			orDash(in.Node),
			orDash(in.Endpoint),
			formatAge(in.Age),
		})
	}
	return renderTable(header, rows)
}

const dash = "-"

func orDash(s string) string {
	if s == "" {
		return dash
	}
	return s
}

// formatAge renders a duration as kubectl does: the single largest unit.
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// renderTable lays out header + rows as a left-aligned, space-padded table with
// a trailing newline. Each column is widened to its longest cell.
func renderTable(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		for i, cell := range cells {
			if i == len(cells)-1 {
				b.WriteString(cell)
			} else {
				b.WriteString(cell)
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
			}
		}
		b.WriteByte('\n')
	}
	writeRow(header)
	for _, row := range rows {
		writeRow(row)
	}
	return b.String()
}
