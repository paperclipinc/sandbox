package main

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// test-agent connects to a guest agent via Firecracker vsock UDS and exercises
// the host->guest data path.
//
// Default mode runs the full ping/exec/files/configure suite (the existing CI
// behavior). The notify mode proves the guest applies a fork notification: it
// sends NotifyForked, then samples the guest CRNG, the guest wall clock, and
// the recorded fork generation, printing labeled lines the workflow can grep.
//
// The read mode is the read-only counterpart used by the husk activate-
// correctness phase: it sends NO NotifyForked (the husk stub already ran the
// fork-correctness handshake during Activate). It only EXECs the already-active
// guest to sample the CRNG, the wall clock, and an optional named env var,
// printing the same URANDOM / WALLCLOCK_NS labels plus ENVVAL so the workflow
// can assert distinct RNG, a stepped clock, and env/secret delivery across two
// independently activated VMs.
//
// Usage:
//
//	test-agent <vsock-uds-path>                              # default suite
//	test-agent --mode notify --generation N <vsock-uds-path> # fork proof
//	test-agent --mode read --read-env KEY <vsock-uds-path>   # post-activate sample
func main() {
	mode := flag.String("mode", "default", "test mode: default | notify | read | egress | name-egress")
	generation := flag.Uint64("generation", 1, "fork generation to send in notify mode")
	readEnv := flag.String("read-env", "", "read mode: name of a guest env var to print as ENVVAL (e.g. a delivered secret or env key)")
	guestIP := flag.String("guest-ip", "", "egress mode: guest eth0 IP to configure (e.g. 10.0.0.2)")
	prefixLen := flag.Int("prefix-len", 30, "egress mode: guest eth0 prefix length")
	gateway := flag.String("gateway", "", "egress mode: guest default gateway (the host tap IP)")
	allowed := flag.String("allowed", "", "egress mode: ip:port the guest MUST be able to reach")
	denied := flag.String("denied", "", "egress mode: ip:port the guest must be BLOCKED from reaching")
	resolver := flag.String("resolver", "", "name-egress mode: DNS resolver IP to write into the guest resolv.conf")
	allowName := flag.String("allow-name", "", "name-egress mode: host:port the guest MUST reach by resolving an allowlisted name (e.g. egress.test:8080)")
	wrongPort := flag.String("wrong-port", "", "name-egress mode: host:port using the allowlisted name on a NON-allowlisted port; must be BLOCKED (e.g. egress.test:9090)")
	deniedName := flag.String("denied-name", "", "name-egress mode: host:port whose name is NOT allowlisted; the resolver must REFUSE it (e.g. denied.test:8080)")
	directIP := flag.String("direct-ip", "", "name-egress mode: ip:port that the allowlisted name resolves to; a DIRECT dial before resolving must be BLOCKED (e.g. 198.51.100.2:8080)")
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
	case "read":
		runRead(client, *readEnv)
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
	case "name-egress":
		runNameEgress(client, nameEgressOpts{
			guestIP:    *guestIP,
			prefixLen:  *prefixLen,
			gateway:    *gateway,
			resolver:   *resolver,
			allowName:  *allowName,
			wrongPort:  *wrongPort,
			deniedName: *deniedName,
			directIP:   *directIP,
		})
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q (want default|notify|read|egress|name-egress)\n", *mode)
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

type nameEgressOpts struct {
	guestIP    string
	prefixLen  int
	gateway    string
	resolver   string
	allowName  string
	wrongPort  string
	deniedName string
	directIP   string
}

// runNameEgress proves NAME-based egress from INSIDE the guest. The host runs a
// controlled DNS resolver that resolves only allowlisted names and pins each
// resolved (ip . port) into this sandbox's nftables set; the guest is pointed at
// that resolver. The proof rests on four honest assertions, each stating exactly
// what it demonstrates:
//
//  1. DIRECT-IP-BEFORE-RESOLVE blocked: dial the destination IP:port directly,
//     BEFORE resolving any name. The IP is not pinned yet, so the host drops it.
//     This proves the static path does not pre-allow the IP; reachability must
//     come from resolving the name. Run first, before any lookup.
//  2. RESOLVE+CONNECT allowed: connect to the allowlisted NAME:port. busybox nc
//     resolves the name through the controlled resolver (which pins the answer)
//     then connects. Success proves resolve-then-pin-then-reach end to end.
//  3. WRONG-PORT blocked: the name resolves, but a connect on a port that is NOT
//     on the allowlist is dropped, because the resolver pinned only the allowed
//     port. Proves pinning is port-scoped.
//  4. DENIED-NAME refused: a name that is NOT allowlisted gets REFUSED by the
//     resolver (no answer), so the guest cannot even resolve it, and the IP it
//     would map to (the same test server) was never pinned for this guest.
//     Proves allowlisting is by NAME, not by the shared destination IP.
//
// The guest cannot influence the host ruleset or the resolver allowlist, so this
// is a genuine end-to-end proof. All values here (IPs, names, ports) are safe to
// log.
func runNameEgress(client *vsock.Client, o nameEgressOpts) {
	if o.guestIP == "" || o.gateway == "" || o.resolver == "" || o.allowName == "" ||
		o.wrongPort == "" || o.deniedName == "" || o.directIP == "" {
		fmt.Fprintln(os.Stderr, "name-egress mode requires --guest-ip, --gateway, --resolver, --allow-name, --wrong-port, --denied-name, --direct-ip")
		os.Exit(1)
	}

	// Bring eth0 up with the per-sandbox /30 address and a default route via the
	// host tap, then point the guest resolver at the controlled DNS resolver IP.
	execOrDie(client, fmt.Sprintf("ip addr add %s/%d dev eth0", o.guestIP, o.prefixLen))
	execOrDie(client, "ip link set eth0 up")
	execOrDie(client, fmt.Sprintf("ip route add default via %s", o.gateway))
	execOrDie(client, fmt.Sprintf("printf 'nameserver %s\\n' > /etc/resolv.conf", o.resolver))
	fmt.Printf("PASS guest-net: eth0=%s/%d gw=%s resolver=%s\n", o.guestIP, o.prefixLen, o.gateway, o.resolver)

	// connectProbe returns the guest exit code of a bounded TCP connect to a
	// host:port. busybox nc resolves host via /etc/resolv.conf (so a NAME goes
	// through the controlled resolver), then connects. Exit 0 == reachable; a
	// dropped SYN hangs until `timeout` kills it (exit 124); a resolve failure
	// also yields a non-zero exit.
	connectProbe := func(label, target string) int {
		host, port := splitHostPort(target)
		out := execOrDie(client, fmt.Sprintf("timeout 6 nc %s %s </dev/null >/dev/null 2>&1; echo $?", host, port))
		code := strings.TrimSpace(out)
		fmt.Printf("name-egress probe [%s] %s -> exit %s\n", label, target, code)
		n := 0
		_, _ = fmt.Sscanf(code, "%d", &n)
		return n
	}

	// lookupProbe returns the guest exit code of a DNS lookup of name through the
	// controlled resolver. busybox nslookup exits non-zero on REFUSED/NXDOMAIN.
	lookupProbe := func(label, name string) int {
		host, _ := splitHostPort(name)
		out := execOrDie(client, fmt.Sprintf("timeout 6 nslookup %s %s >/dev/null 2>&1; echo $?", host, o.resolver))
		code := strings.TrimSpace(out)
		fmt.Printf("name-egress lookup [%s] %s -> exit %s\n", label, host, code)
		n := 0
		_, _ = fmt.Sscanf(code, "%d", &n)
		return n
	}

	// 1. Direct dial to the destination IP BEFORE resolving any name must be
	// blocked: nothing is pinned, so the host drops it. Ordering matters; this
	// runs before any lookup.
	if rc := connectProbe("direct-ip-before-resolve", o.directIP); rc == 0 {
		fmt.Fprintf(os.Stderr, "FAIL name-egress: direct dial to %s was reachable BEFORE resolving; the IP is statically allowed, not name-gated\n", o.directIP)
		os.Exit(1)
	}
	fmt.Printf("PASS name-egress: direct IP %s blocked before any name is resolved\n", o.directIP)

	// 2. Resolve + connect to the allowlisted name on the allowed port: the
	// resolver answers and pins, so the guest reaches it.
	if rc := connectProbe("allow-name", o.allowName); rc != 0 {
		fmt.Fprintf(os.Stderr, "FAIL name-egress: allowlisted name %s was NOT reachable (exit %d); resolve+pin+reach broken\n", o.allowName, rc)
		os.Exit(1)
	}
	fmt.Printf("PASS name-egress: allowlisted name %s reachable (resolved and pinned)\n", o.allowName)

	// 3. The allowlisted name on a NON-allowlisted port must be blocked even
	// though the name resolves: the resolver pinned only the allowed port.
	if rc := connectProbe("wrong-port", o.wrongPort); rc == 0 {
		fmt.Fprintf(os.Stderr, "FAIL name-egress: wrong port %s WAS reachable; pinning is not port-scoped\n", o.wrongPort)
		os.Exit(1)
	}
	fmt.Printf("PASS name-egress: allowlisted name on wrong port %s blocked\n", o.wrongPort)

	// 4. A name that is NOT allowlisted must be REFUSED by the resolver: the
	// guest cannot resolve it, and the (shared) IP it would map to was never
	// pinned for this guest. The lookup failing proves the refusal; the connect
	// failing confirms it is not reachable by name.
	if rc := lookupProbe("denied-name", o.deniedName); rc == 0 {
		fmt.Fprintf(os.Stderr, "FAIL name-egress: non-allowlisted name %s was RESOLVED; the resolver did not refuse it\n", o.deniedName)
		os.Exit(1)
	}
	if rc := connectProbe("denied-name", o.deniedName); rc == 0 {
		fmt.Fprintf(os.Stderr, "FAIL name-egress: non-allowlisted name %s was reachable; refusal did not block egress\n", o.deniedName)
		os.Exit(1)
	}
	fmt.Printf("PASS name-egress: non-allowlisted name %s refused and unreachable (allowlisting is by name, not by IP)\n", o.deniedName)

	fmt.Println("PASS name-egress: controlled resolver enforces name-based egress end to end")
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

// runRead is the read-only sampler for the husk activate-correctness phase. The
// husk stub already ran the fork-correctness handshake (NotifyForked reseed +
// clock step) and delivered env/secrets via Configure during Activate, so this
// mode sends NO NotifyForked: it only EXECs the already-active guest to sample
// the CRNG (URANDOM) and the wall clock (WALLCLOCK_NS), and, when readEnv is
// set, prints the value of that guest env var as ENVVAL so the workflow can
// assert a delivered env/secret reached the guest. The workflow then asserts the
// URANDOM samples from two independently activated VMs DIFFER (distinct reseed),
// each WALLCLOCK_NS is near the runner clock (stepped from the frozen snapshot),
// and ENVVAL matches the delivered value (env/secret delivery). The delivered
// value is printed only because the workflow must read it back from the guest;
// it is the SAME value the workflow then greps the host stub log to confirm is
// absent there. test-agent prints to its own stdout, not the stub log.
func runRead(client *vsock.Client, readEnv string) {
	urandom := execOrDie(client, "head -c 32 /dev/urandom | base64 | tr -d '\\n'")
	wallclock := execOrDie(client, "date +%s%N")
	fmt.Printf("URANDOM=%s\n", strings.TrimSpace(urandom))
	fmt.Printf("WALLCLOCK_NS=%s\n", strings.TrimSpace(wallclock))

	if readEnv != "" {
		// printf with no trailing newline so ENVVAL is the exact value. The env
		// var is referenced indirectly so the var NAME, not a literal, is what is
		// printed by the guest shell.
		val := execOrDie(client, fmt.Sprintf("printf '%%s' \"$%s\"", readEnv))
		fmt.Printf("ENVVAL=%s\n", strings.TrimSpace(val))
	}
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
