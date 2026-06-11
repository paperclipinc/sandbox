// Command tmpl-smoke drives the real KVM-backed fork engine end to end to prove
// the image-to-rootfs pipeline: it builds a Firecracker template FROM AN OCI
// IMAGE (pull -> flatten -> inject agent -> ext4 -> boot -> run init in the VM
// -> snapshot), forks a sandbox from that template, and execs assertions over
// the guest agent that prove BOTH the init command ran (a file it wrote exists
// with the expected content) AND the image filesystem is present (an
// image-specific binary resolves). It exists so KVM CI has a single binary that
// genuinely exercises Engine.CreateTemplate's image build, not a hand-built
// rootfs.
//
// Usage:
//
//	tmpl-smoke \
//	  --image busybox:stable \
//	  --init 'echo built-by-init > /built.txt' \
//	  --data-dir /tmp/smoke \
//	  --firecracker /usr/local/bin/firecracker \
//	  --kernel /tmp/vmlinux \
//	  --agent-bin /tmp/agent \
//	  --busybox-bin /tmp/busybox \
//	  --expect-file /built.txt --expect-content built-by-init \
//	  --expect-cmd 'ls /bin/busybox'
//
// Every assertion gates: any failure exits nonzero so the CI step fails.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/paperclipinc/sandbox/internal/firecracker"
	"github.com/paperclipinc/sandbox/internal/fork"
	"github.com/paperclipinc/sandbox/internal/vsock"
)

func main() {
	image := flag.String("image", "", "OCI image reference to build the template from (e.g. busybox:stable)")
	initCmd := flag.String("init", "", "init command to run IN the booted template VM before snapshot")
	dataDir := flag.String("data-dir", "", "engine data directory")
	fcBin := flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
	kernel := flag.String("kernel", "", "path to the guest kernel (vmlinux)")
	agentBin := flag.String("agent-bin", "", "path to the guest agent binary injected as /init")
	busyboxBin := flag.String("busybox-bin", "", "path to a static busybox injected as /bin/sh when the image lacks a shell")
	expectFile := flag.String("expect-file", "", "file the init command must have created in the VM")
	expectContent := flag.String("expect-content", "", "substring the expect-file must contain")
	expectCmd := flag.String("expect-cmd", "", "a command that must succeed in the fork, proving the image filesystem is present")
	flag.Parse()

	if *image == "" || *dataDir == "" || *kernel == "" || *agentBin == "" {
		fmt.Fprintln(os.Stderr, "tmpl-smoke: --image, --data-dir, --kernel and --agent-bin are required")
		os.Exit(2)
	}

	if err := run(opts{
		image:         *image,
		initCmd:       *initCmd,
		dataDir:       *dataDir,
		fcBin:         *fcBin,
		kernel:        *kernel,
		agentBin:      *agentBin,
		busyboxBin:    *busyboxBin,
		expectFile:    *expectFile,
		expectContent: *expectContent,
		expectCmd:     *expectCmd,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "tmpl-smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("tmpl-smoke: PASS: OCI image -> ext4 -> boot -> init -> snapshot -> fork -> exec proven")
}

type opts struct {
	image, initCmd, dataDir, fcBin, kernel, agentBin, busyboxBin string
	expectFile, expectContent, expectCmd                         string
}

func run(o opts) error {
	engine, err := fork.NewEngine(o.dataDir, o.fcBin, o.kernel, firecracker.JailerConfig{}, fork.EngineOpts{
		// This template snapshot is built by THIS process, so its digest is
		// recorded and the verified marker written; AllowUnverified is belt and
		// suspenders for a single-process in-CI run.
		AllowUnverified: true,
		AgentBinPath:    o.agentBin,
		BusyboxPath:     o.busyboxBin,
	})
	if err != nil {
		return fmt.Errorf("new engine: %w", err)
	}

	var initCommands []string
	if o.initCmd != "" {
		initCommands = []string{o.initCmd}
	}

	templateID := "smoke-tmpl"
	fmt.Printf("tmpl-smoke: building template %q from image %q (init=%v)\n", templateID, o.image, initCommands)
	buildStart := time.Now()
	if err := engine.CreateTemplate(templateID, o.image, initCommands); err != nil {
		return fmt.Errorf("create template from image: %w", err)
	}
	fmt.Printf("tmpl-smoke: template built in %s\n", time.Since(buildStart).Round(time.Millisecond))

	sandboxID := "smoke-fork-1"
	fmt.Printf("tmpl-smoke: forking sandbox %q from template\n", sandboxID)
	res, err := engine.Fork(templateID, sandboxID, fork.ForkOpts{})
	if err != nil {
		return fmt.Errorf("fork from template: %w", err)
	}
	defer func() { _ = engine.Terminate(sandboxID) }()
	fmt.Printf("tmpl-smoke: forked in %.2fms, vsock=%s\n", res.ForkTimeMs, res.VsockPath)

	client, err := connect(res.VsockPath)
	if err != nil {
		return fmt.Errorf("connect to forked guest agent: %w", err)
	}
	defer client.Close()

	// Assertion 1: the init command ran IN the template VM at build time, so the
	// file it wrote is present in the forked sandbox (the snapshot captured it).
	if o.expectFile != "" {
		out, err := execOK(client, fmt.Sprintf("cat %s", o.expectFile))
		if err != nil {
			return fmt.Errorf("init-command proof: reading %s failed (init never ran?): %w", o.expectFile, err)
		}
		if o.expectContent != "" && !strings.Contains(out, o.expectContent) {
			return fmt.Errorf("init-command proof: %s = %q, want it to contain %q", o.expectFile, strings.TrimSpace(out), o.expectContent)
		}
		fmt.Printf("tmpl-smoke: init-command proof OK: %s contains %q\n", o.expectFile, o.expectContent)
	}

	// Assertion 2: the image filesystem is actually present (an image-specific
	// command resolves), proving the OCI layers were flattened into the rootfs
	// and not just an empty ext4 with the agent.
	if o.expectCmd != "" {
		if _, err := execOK(client, o.expectCmd); err != nil {
			return fmt.Errorf("image-filesystem proof: %q failed (image not extracted?): %w", o.expectCmd, err)
		}
		fmt.Printf("tmpl-smoke: image-filesystem proof OK: %q succeeded\n", o.expectCmd)
	}

	return nil
}

// execOK runs a command in the fork over the guest agent and returns its stdout,
// failing if the transport errors or the command exits nonzero.
func execOK(client *vsock.Client, command string) (string, error) {
	res, err := client.Exec(command, "/", nil, 60)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("command %q exited %d: %s", command, res.ExitCode, res.Stderr)
	}
	return res.Stdout, nil
}

// connect dials the forked guest agent over vsock with a bounded retry while the
// restored VM finishes coming up.
func connect(udsPath string) (*vsock.Client, error) {
	var client *vsock.Client
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		client, err = vsock.Connect(udsPath, vsock.AgentPort)
		if err == nil {
			if _, perr := client.Ping(); perr == nil {
				return client, nil
			} else {
				_ = client.Close()
				err = perr
			}
		}
		time.Sleep(1 * time.Second)
	}
	return nil, fmt.Errorf("connect after retries: %w", err)
}
