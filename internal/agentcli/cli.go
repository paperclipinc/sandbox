package agentcli

import (
	"context"
	"fmt"
	"io"
)

const usage = `agentrun: snapshot-fork sandboxes for AI agents

Usage:
  agentrun run <command> [--pool P] [--timeout N]   create a sandbox, run the
                                                    command, terminate, and exit
                                                    with the command's exit code
  agentrun sandbox create [--pool P]                create a sandbox, print its id
  agentrun sandbox ls [-n namespace] [-A]           list sandboxes
  agentrun sandbox exec <id> <command...>           run a command in a sandbox
  agentrun sandbox fork <id> [--replicas N]         fork a sandbox, print new ids
  agentrun sandbox terminate <id>                   destroy a sandbox
  agentrun dev up | down                            bring a local kind dev
                                                    cluster up or down

Flags:
  --pool string      pool to create sandboxes from
  --timeout int      exec timeout in seconds (0 = backend default)
  -n string          namespace (ls)
  -A                 all namespaces (ls)
  --replicas int     number of forks (fork)
  -h, --help         print this help
`

// Run is the testable CLI entry point. It dispatches args (without the program
// name) against backend, writing normal output to out and diagnostics to errw,
// and returns a process exit code:
//
//	0  success (for run: the command's exit code)
//	2  usage error (unknown subcommand, missing argument, bad flag)
//	1  a backend or runtime error
//
// For run, the exit code is the executed command's exit code so callers can
// chain agentrun in shell pipelines.
func Run(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errw, usage)
		return 2
	}

	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(out, usage)
		return 0
	case "run":
		return cmdRun(ctx, args[1:], backend, out, errw)
	case "sandbox":
		return cmdSandbox(ctx, args[1:], backend, out, errw)
	case "dev":
		return cmdDev(ctx, args[1:], out, errw)
	default:
		fmt.Fprintf(errw, "unknown subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}
