package main

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/paperclipinc/sandbox/internal/vsock"
)

// test-agent connects to a guest agent via Firecracker vsock UDS and exercises
// the host->guest data path.
//
// Default mode runs the full ping/exec/files/configure suite (the existing CI
// behavior). The notify mode proves the guest applies a fork notification: it
// sends NotifyForked, then samples the guest CRNG, the guest wall clock, and
// the recorded fork generation, printing labeled lines the workflow can grep.
//
// Usage:
//
//	test-agent <vsock-uds-path>                              # default suite
//	test-agent --mode notify --generation N <vsock-uds-path> # fork proof
func main() {
	mode := flag.String("mode", "default", "test mode: default | notify | egress")
	generation := flag.Uint64("generation", 1, "fork generation to send in notify mode")
	guestIP := flag.String("guest-ip", "", "egress mode: guest eth0 IP to configure (e.g. 10.0.0.2)")
	prefixLen := flag.Int("prefix-len", 30, "egress mode: guest eth0 prefix length")
	gateway := flag.String("gateway", "", "egress mode: guest default gateway (the host tap IP)")
	allowed := flag.String("allowed", "", "egress mode: ip:port the guest MUST be able to reach")
	denied := flag.String("denied", "", "egress mode: ip:port the guest must be BLOCKED from reaching")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: test-agent [--mode default|notify|egress] [flags] <vsock-uds-path>")
		os.Exit(1)
	}
	udsPath := args[0]

	client := connect(udsPath)
	defer client.Close()

	switch *mode {
	case "notify":
		runNotify(client, *generation)
	case "default":
		runDefault(client)
	case "egress":
		runEgress(client, egressOpts{
			guestIP:   *guestIP,
			prefixLen: *prefixLen,
			gateway:   *gateway,
			allowed:   *allowed,
			denied:    *denied,
		})
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q (want default|notify|egress)\n", *mode)
		os.Exit(1)
	}
}

type egressOpts struct {
	guestIP   string
	prefixLen int
	gateway   string
	allowed   string
	denied    string
}

// runEgress proves host-side nftables egress enforcement from INSIDE the guest.
// It configures eth0 statically (the workflow boots a fresh VM whose NIC is on
// the per-sandbox tap, so the host-side allowlist is already in force), then
// uses the guest agent's exec to probe two destinations:
//
//	allowed ip:port -> the TCP connect MUST succeed (a local listener answers)
//	denied  ip:port -> the TCP connect MUST be blocked (nft drops the SYN, so
//	                   the connect attempt times out)
//
// The guest cannot influence the host ruleset, so a success on allowed plus a
// block on denied is a genuine end-to-end proof that the default-deny egress
// allowlist is enforced host-side. All values here (IPs, ports) are safe to log.
func runEgress(client *vsock.Client, o egressOpts) {
	if o.guestIP == "" || o.gateway == "" || o.allowed == "" || o.denied == "" {
		fmt.Fprintln(os.Stderr, "egress mode requires --guest-ip, --gateway, --allowed, --denied")
		os.Exit(1)
	}

	// Bring eth0 up with the per-sandbox /30 address and a default route via the
	// host tap. busybox ip is present in the CI rootfs.
	execOrDie(client, fmt.Sprintf("ip addr add %s/%d dev eth0", o.guestIP, o.prefixLen))
	execOrDie(client, "ip link set eth0 up")
	execOrDie(client, fmt.Sprintf("ip route add default via %s", o.gateway))
	fmt.Printf("PASS guest-net: eth0=%s/%d gw=%s\n", o.guestIP, o.prefixLen, o.gateway)

	// probe returns the guest-side exit code of a bounded TCP connect attempt.
	// busybox nc connects and immediately sees EOF (exit 0) when allowed; when
	// the SYN is dropped it hangs until `timeout` kills it (exit 124).
	probe := func(target string) int {
		host, port := splitHostPort(target)
		out := execOrDie(client, fmt.Sprintf("timeout 5 nc %s %s </dev/null >/dev/null 2>&1; echo $?", host, port))
		code := strings.TrimSpace(out)
		fmt.Printf("egress probe %s -> exit %s\n", target, code)
		n := 0
		_, _ = fmt.Sscanf(code, "%d", &n)
		return n
	}

	if rc := probe(o.allowed); rc != 0 {
		fmt.Fprintf(os.Stderr, "FAIL egress: allowed destination %s was NOT reachable (exit %d); host allowlist is over-blocking\n", o.allowed, rc)
		os.Exit(1)
	}
	fmt.Printf("PASS egress: allowed destination %s reachable\n", o.allowed)

	if rc := probe(o.denied); rc == 0 {
		fmt.Fprintf(os.Stderr, "FAIL egress: denied destination %s WAS reachable; host default-deny is not enforced\n", o.denied)
		os.Exit(1)
	}
	fmt.Printf("PASS egress: denied destination %s blocked\n", o.denied)
	fmt.Println("PASS egress: host-side default-deny allowlist enforced end to end")
}

// splitHostPort splits an ip:port string into its host and port parts. It does
// not validate; the workflow passes well-formed values.
func splitHostPort(s string) (host, port string) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

// connect retries while the guest agent finishes starting.
func connect(udsPath string) *vsock.Client {
	var client *vsock.Client
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		client, err = vsock.Connect(udsPath, vsock.AgentPort)
		if err == nil {
			return client
		}
		fmt.Printf("connect attempt %d failed: %v (retrying in 2s)\n", attempt+1, err)
		time.Sleep(2 * time.Second)
	}
	fmt.Fprintf(os.Stderr, "connect failed after 10 attempts: %v\n", err)
	os.Exit(1)
	return nil
}

// runNotify sends a fork notification and prints proof lines for the workflow:
// URANDOM, WALLCLOCK_NS, and FORKGEN. It does NOT exercise forkd; sending
// NotifyForked directly is the unit-level proof that the guest applies a
// reseed/clock step. forkd end-to-end notify is covered by go tests.
func runNotify(client *vsock.Client, generation uint64) {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL rand: %v\n", err)
		os.Exit(1)
	}

	resp, err := client.NotifyForked(generation, entropy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL notify_forked: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PASS notify_forked: reseeded_rng=%v clock_step_ns=%d signaled=%d\n",
		resp.ReseededRNG, resp.AppliedClockStepNanos, resp.SignaledProcesses)

	urandom := execOrDie(client, "head -c 32 /dev/urandom | base64 | tr -d '\\n'")
	wallclock := execOrDie(client, "date +%s%N")
	forkgen := execOrDie(client, "cat /run/sandbox/fork-generation")

	fmt.Printf("URANDOM=%s\n", strings.TrimSpace(urandom))
	fmt.Printf("WALLCLOCK_NS=%s\n", strings.TrimSpace(wallclock))
	fmt.Printf("FORKGEN=%s\n", strings.TrimSpace(forkgen))
}

// execOrDie runs a shell command in the guest and returns stdout, failing hard
// on a transport error or non-zero exit.
func execOrDie(client *vsock.Client, cmd string) string {
	result, err := client.Exec(cmd, "/workspace", nil, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL exec %q: %v\n", cmd, err)
		os.Exit(1)
	}
	if result.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "FAIL exec %q: exit_code=%d stderr=%q\n", cmd, result.ExitCode, result.Stderr)
		os.Exit(1)
	}
	return result.Stdout
}

// runDefault is the original ping/exec/files/configure suite.
func runDefault(client *vsock.Client) {
	// Test ping
	uptime, err := client.Ping()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL ping: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PASS ping: uptime=%.2fs\n", uptime)

	// Test exec
	result, err := client.Exec("echo hello from sandbox", "/workspace", nil, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL exec: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PASS exec: exit_code=%d stdout=%q exec_time=%.2fms\n",
		result.ExitCode, result.Stdout, result.ExecTimeMs)

	// Test write + read file
	err = client.WriteFile("/workspace/test.txt", []byte("hello sandbox"), 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL write: %v\n", err)
		os.Exit(1)
	}
	content, err := client.ReadFile("/workspace/test.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL read: %v\n", err)
		os.Exit(1)
	}
	if string(content) != "hello sandbox" {
		fmt.Fprintf(os.Stderr, "FAIL read: expected %q, got %q\n", "hello sandbox", string(content))
		os.Exit(1)
	}
	fmt.Printf("PASS files: wrote and read back %q\n", string(content))

	// Test list dir
	entries, err := client.ListDir("/workspace")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL listdir: %v\n", err)
		os.Exit(1)
	}
	data, _ := json.Marshal(entries)
	fmt.Printf("PASS listdir: %s\n", string(data))

	// Test configure: claim-time env+secrets must reach exec sessions.
	if err := client.Configure(
		map[string]string{"CFG_VAR": "cfg"},
		map[string]string{"TEST_SECRET": "s3cr3t-canary"},
	); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL configure: %v\n", err)
		os.Exit(1)
	}
	result, err = client.Exec(`echo -n "$CFG_VAR:$TEST_SECRET"`, "/workspace", nil, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL exec after configure: %v\n", err)
		os.Exit(1)
	}
	if result.Stdout != "cfg:s3cr3t-canary" {
		fmt.Fprintf(os.Stderr, "FAIL configure: stdout=%q, want cfg:s3cr3t-canary\n", result.Stdout)
		os.Exit(1)
	}
	// Per-request env must override configured values.
	result, err = client.Exec(`echo -n "$CFG_VAR"`, "/workspace", map[string]string{"CFG_VAR": "override"}, 10)
	if err != nil || result.Stdout != "override" {
		fmt.Fprintf(os.Stderr, "FAIL configure precedence: err=%v stdout=%q\n", err, result.Stdout)
		os.Exit(1)
	}
	fmt.Println("PASS configure: env+secrets visible to exec, request overrides configured")

	fmt.Println("")
	fmt.Println("================================")
	fmt.Println("  All guest agent tests passed!")
	fmt.Println("================================")
}
