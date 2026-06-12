# Guest networking

Guest networking is opt-in per node and gives each sandbox its own network
identity and a host-side, default-deny egress allowlist. The guest can never
influence its own policy and cannot spoof another sandbox's traffic. This
document describes what is ENFORCED today, what is still OPEN, and why the
isolation holds.

Enable it on a node with `forkd --enable-networking` (off by default). Related
flags: `--sandbox-subnet` (IPv4 subnet carved into /30s, default
`10.200.0.0/16`), `--uplink` (host egress interface for an optional MASQUERADE
rule; empty relies on the node's existing NAT), `--dns-resolver` (a resolver IP
guests may reach; empty omits the DNS allow rule). With both the network
Manager and the identity allocator wired, every fork that carries a
`NetworkPolicy` gets a distinct identity; with networking off the engine behaves
exactly as before.

## Tap-per-sandbox identity

Each sandbox gets a unique identity from `internal/netconf.Allocator`, which
carves `--sandbox-subnet` into /30 point-to-point blocks:

- a **tap** device (`<prefix><8 hex>` derived from the sandbox id, always within
  the kernel's 15-char interface-name limit),
- a **/30** whose two usable addresses are the host side (the tap's IP, the
  guest's gateway) and the guest side (the guest's eth0 IP),
- a locally-administered unicast **MAC**.

All of these are deterministic from the sandbox id and safe to log. Acquire is
idempotent per id; Release frees the /30 block for reuse.

A template snapshot is built with a placeholder NIC baked in (Firecracker cannot
add a NIC on restore). Every fork remaps that baked NIC to its OWN tap at load
time via Firecracker `network_overrides` (the v1.15 network analog of the
relative vsock `uds_path` used for fork-correctness): same baked iface id,
distinct `host_dev_name` per fork. So two forks of one snapshot never share a
tap.

## Fresh identity per fork

`Engine.Fork` acquires a fresh identity, calls the network Manager's `Setup`
(create tap, assign host IP, bring it up, install the egress ruleset), builds
the `network_overrides` that bind the baked NIC to this fork's tap, and delivers
the per-fork guest config (guest IP, gateway, prefix length) to the guest agent
in the `NotifyForked` vsock message. The guest reconfigures its eth0 to the new
address on every fork. `Terminate` tears the tap and ruleset down and releases
the /30.

`ForkRunning` (live fork) of a networked sandbox **fails closed**: a live fork
restores the source's baked NIC, which would collide on tap/MAC/IP with the
source's live network. Until each fork's interface is isolated in its own
per-VM network namespace (husk pods, #18), live-forking a networked sandbox is
unsupported and returns an error rather than producing a colliding VM.

## nftables dispatch model

All sandboxes on a node share ONE `inet` table (`agentrun_egress`) so that
adding or removing one sandbox never disturbs another's traffic. The shape:

- **One shared table** with **one base chain** hooked on the `forward` path,
  **policy accept**. The base chain never drops; it only dispatches.
- **One verdict map** (`tapdispatch`) keyed by inbound interface name. The base
  chain's single rule looks up the inbound interface (the tap) in the map and
  jumps to that sandbox's regular chain. Interfaces not in the map fall through
  the accept policy untouched, so unrelated host forwarding is unaffected.
- **One regular chain per sandbox** (`sb_<tap>`), reached only via the dispatch
  jump for that tap. It accepts established/related, then each allowlisted
  `ip daddr <dest> tcp dport <port>`, then (optionally) DNS to the configured
  resolver, and ends in a **terminal drop** (under `egress: deny`) or accept
  (under `egress: allow`). Every accept is pinned to `ip saddr <guestIP>` as
  anti-spoof.

### Why cross-tap isolation holds

The drop that enforces default-deny lives in the per-sandbox regular chain, not
in a shared base chain. Because that chain is reached ONLY through the per-tap
dispatch jump, its terminal drop is a verdict for that one sandbox's packets and
cannot terminate another sandbox's allowed traffic. This is the fix for an
earlier design where a single shared policy-drop base chain on the forward hook
made one sandbox's drop terminal for every tap. Installing a second sandbox
re-applies the shared skeleton (idempotent `add` of named objects; the base
chain's single dispatch rule is the only thing flushed-and-re-added, and it
holds no per-sandbox state) and then adds only that sandbox's chain and map
element, so it never flushes the first sandbox's chain.

The `ip saddr` pin on every accept is defense in depth: even though only one tap
reaches a given chain, the source-address check stops a guest from spoofing
another sandbox's source IP onto its own tap.

This is exactly the model `cmd/net-smoke` installs through the real Manager in
KVM CI, and the rendering lives in `internal/netconf` (rendered ruleset) and
`internal/network` (ordered `ip`/`nft` orchestration with an injected exec
runner). The Go unit tests assert command order and idempotency with a fake
runner; the darwin gap (no `nft` to accept the rendered syntax) is closed by the
KVM CI phases below.

## Name-based egress: the controlled DNS resolver

A literal `ip daddr . tcp dport` rule cannot enforce a NAME, because nftables
matches on the resolved IP and never sees the name the guest looked up. The
`--enable-dns-egress` path closes this with a controlled per-node resolver
(`internal/dnsproxy`, #47). The design:

- **Single node resolver IP.** forkd binds the controlled resolver on a node-wide
  address (`--dns-resolver`, default `169.254.1.1`) on `udp/tcp 53`. Every
  sandbox chain allows DNS to exactly that IP and no other, so a guest cannot
  reach any external resolver (no DoH/DoT bypass: an external resolver's IP:port
  is not allowlisted and was never pinned).
- **Guest points at it.** The guest agent writes `/etc/resolv.conf` with
  `nameserver <resolverIP>` on configure, so the guest's only resolver is the one
  we control.
- **Source-IP attribution.** The resolver maps each query to a sandbox by its
  source guest IP (each sandbox has a unique /30 from the identity allocator) and
  consults THAT sandbox's name allowlist. A query whose source has no live tap
  mapping is REFUSED and pins nothing.
- **Allowlist names: exact OR anchored suffix wildcard.** A name entry matches a
  query exactly (case-insensitive, trailing-dot tolerant) OR, when written `*.D`,
  by the ANCHOR RULE: the query must end with `.D` AND have a non-empty label
  before that `.D`. So `*.example.com` matches `a.example.com` and
  `a.b.example.com` but NEVER the apex `example.com`, NEVER a look-alike
  (`notexample.com`, `evilexample.com`, `xexample.com`), and NEVER a name that
  carries `D` only as a non-suffix label (`example.com.evil.com`). The match is a
  LITERAL anchored suffix check (no regex). Exact and wildcard entries coexist; a
  name matching both gets the union of their ports. A wildcard is validated where
  the template names build the allowlist (`netconf.ParseNameAllowList`): it must
  be exactly a single leading `*.` plus a valid domain, so `*`, `*.`, `*foo.com`,
  `a.*.com`, `**.com`, and multi-star names are REJECTED at the boundary, never
  silently treated as a literal.
- **Resolve-then-pin (A and AAAA).** For an allowlisted name, the resolver
  forwards the query to the configured upstream (`--dns-upstream`), and for each
  A record it pins `(recordIP . allowedPort)` into that sandbox's v4 timeout set
  (`ipv4_addr . inet_service`, `flags timeout`) and for each AAAA record into a
  SEPARATE v6 timeout set (`ipv6_addr . inet_service`, `flags timeout`), each
  with a timeout of `max(recordTTL, 30s floor)`, then returns the SAME answer to
  the guest. Because the guest connects to exactly the IP the resolver pinned,
  the guest and the firewall always agree on the address with no resolve-and-pin
  race. A name NOT on the allowlist gets REFUSED; nothing is pinned.
- **Port-scoped, saddr-pinned (v4).** Only the allowed ports are pinned, and the
  v4 dynamic-set accept rule (like every other v4 accept) is `ip saddr <guestIP>`
  pinned, so a spoofed-source query cannot land a pin the spoofing guest can use.
- **TTL window.** A pinned `(ip . port)` stays reachable for the timeout above
  even after the name stops resolving to it; the set evicts the element when the
  timeout lapses. The 30s floor keeps a very short TTL from expiring a pin before
  the guest connects.

AAAA/IPv6 is enforced by the same resolve-then-pin model: a resolved v6 address
is pinned into the per-sandbox v6 set, and each per-sandbox chain carries a v6
default-deny (`meta nfproto ipv6 drop` under `egress: deny`) so an unpinned v6
destination is dropped rather than falling through to the base chain's accept.
Honest scope: the guest is assigned only a v4 `/30` source identity today (no v6
source address), so the v6 accept is not `ip saddr` anti-spoof-pinned like the
v4 accepts; in single-stack guests this is moot (the guest cannot source v6) and
the dataplane fails closed regardless via the v6 default-deny. The residual risks
(upstream-resolver trust, the bounded TTL window, the shared-CDN-IP caveat) are
documented per row in `docs/threat-model.md`.

Per mode: in raw-forkd mode this dnsproxy + per-tap nftables IS the egress
mechanism over the VM tap; in the husk default the VM tap lives in the husk
pod's netns and a Kubernetes NetworkPolicy/Cilium over that pod is the governing
egress layer (no bespoke nftables for husk pods). Exactly one layer governs a
given sandbox, never both (threat model section 0, surface 5).

## Enforced vs open

**ENFORCED (proven in KVM CI):**

- Host-side default-deny egress for literal **IP:port** allowlist entries. The
  guest cannot edit the ruleset (its only network config is its own eth0) and
  cannot spoof another sandbox's source address (per-tap dispatch + `ip saddr`).
- Two-sandbox **cross-tap isolation**: `cmd/net-smoke validate` installs two
  identities through the real Manager and asserts `nft list ruleset` has both
  per-sandbox chains and a dispatch map keying both taps to their own chains.
- **Single-VM enforcement**: a Firecracker VM whose NIC is on a per-sandbox tap
  reaches an ALLOWED destination IP:port and is BLOCKED from a DENIED one
  (the denied connect times out), driven from inside the guest over the guest
  agent. The destination lives in a separate netns reached over a veth, so the
  traffic is genuinely forwarded through the host's nftables forward hook.
- **Name-based egress** (exact FQDNs and anchored suffix wildcards `*.D`, behind
  `--enable-dns-egress`). A sandbox allowed the NAME `egress.test:8080` resolves
  it through the controlled resolver and reaches the resolved IP on that port;
  the right name on a wrong port, a name not on the allowlist (REFUSED), and a
  direct dial to the destination IP without first resolving the name are all
  BLOCKED. Proven from inside the guest in KVM CI against a stub upstream that
  maps the allowlisted and a denied name to the SAME IP, so the proof is by
  NAME, not by IP. The wildcard anchor matcher and the A/AAAA pin paths are unit
  bypass-tested in `internal/dnsproxy` and `internal/netconf`; the v4 in-VM proof
  is the CI-proven path, and the v6 dataplane is exercised by the chain-render
  and pinner tests.

**OPEN (not enforced; documented, not pretended):**

- **Suffix/wildcard name matching** (e.g. `*.anthropic.com`). v1 is exact-match
  FQDN only; a wildcard allow entry is not expanded.
- **AAAA/IPv6 name egress.** v1 is A/IPv4 only; the resolver answers AAAA with an
  empty NOERROR and pins no IPv6. IPv6 egress is a follow-up.
- **Upstream-resolver trust, per-name rate limiting, DNSSEC.** The proxy pins
  whatever its configured upstream returns; a malicious upstream can point an
  allowlisted name at an attacker IP. There is no per-name rate limit and no
  DNSSEC validation in v1 (see `docs/threat-model.md`).
- **Snapshot-fork networking under a per-VM netns.** Live-fork fails closed;
  per-VM netns isolation lands with husk pods (#18).
- **Per-fork conntrack flush**, parent-connection-death semantics beyond
  fresh-identity, bandwidth/rate limiting, and IPv6.

## Layering: host netns vs per-VM netns

Today the tap and the nftables ruleset live in forkd's (the host's) network
namespace. Isolation between sandboxes is by per-tap dispatch + per-/30
addressing + `ip saddr` anti-spoof, not by a kernel netns boundary per VM.
Moving each VM into its own pod network namespace (husk pods, #18) adds a
second, defense-in-depth layer and is where snapshot-fork-under-netns is
resolved. The two layers are complementary: the host-side allowlist is the
policy boundary; the per-VM netns is the containment boundary.

## Per-mode egress enforcement: husk vs raw-forkd

There are two run modes (see [docs/husk-pods.md](husk-pods.md)) and they enforce
egress through DIFFERENT mechanisms. The two do NOT both govern a given
sandbox; exactly one applies, decided by the run mode.

- **Husk mode (the default).** The VM's tap lives in the HUSK POD's network
  namespace (the VM runs inside the pod). The pod netns is therefore the egress
  boundary, and a Kubernetes `NetworkPolicy` (or Cilium) selecting the husk pod
  is the GOVERNING egress layer: it is enforced by the CNI on the pod's netns,
  exactly as for any pod. This is honest pod networking: the sandbox's traffic is
  the pod's traffic, so the cluster's existing pod-network policy machinery
  applies with zero bespoke code. The bespoke host-nftables engine described
  above is REDUNDANT for the husk pod-netns path and is NOT installed for husk
  pods.
- **Raw-forkd mode (`--enable-raw-forkd`, and `--mock`).** There is no pod; the
  VM's tap lives on the HOST (forkd's netns). A Kubernetes `NetworkPolicy` cannot
  see a host-netns tap, so the bespoke host-nftables egress engine
  (`internal/network` + `internal/netconf`, the default-deny per-tap allowlist,
  issues #47/#48) plus the controlled DNS resolver (`internal/dnsproxy`) ARE the
  enforcement mechanism. This is the engine the rest of this document describes.

So the bespoke nftables engine is RETAINED, because raw-forkd still needs it; it
is the only egress enforcement on the host-tap path. It is redundant only in husk
mode, where the pod netns + `NetworkPolicy` govern instead. Neither mode runs
both layers over the same sandbox.

Honest scope: in husk mode the `NetworkPolicy` is the policy boundary at the
OBJECT level (it selects the husk pod and the CNI is responsible for enforcement),
proven object-level on kind in the conformance job. The actual in-VM enforcement
of the VM tap by the pod netns requires a KVM-capable kubelet running the husk
pod's VMM (a bare-metal reference node); that in-VM enforcement is not gated on
the shared kind runner (the nested VMM does not reliably come up there) and is
the documented open item.

## CI proof

`.github/workflows/kvm-test.yaml` runs three REQUIRED (gating) phases on a root
Linux runner with `nft` + `iproute2`:

1. **Host nftables two-sandbox install validation against real nft.** Builds and
   runs `cmd/net-smoke validate`, which drives the real `internal/network`
   Manager for two identities and asserts the live ruleset proves cross-tap
   isolation. This is the phase that closes the darwin no-`nft` gap.
2. **Guest egress enforcement.** Brings up one tap + /30 + nftables allowlist via
   `cmd/net-smoke setup-one`, boots a Firecracker VM with a NIC on that tap and
   the existing agent rootfs, then via the guest agent (`test-agent --mode
   egress`) configures eth0 and asserts the allowed destination is reachable and
   the denied destination is blocked.
3. **Name-based egress enforcement.** Brings up one tap whose allowlist is the
   NAME `egress.test:8080`, binds the controlled resolver on `169.254.1.1`, runs
   it via `cmd/net-smoke setup-name-egress` with a stub upstream (`cmd/dns-stub`)
   that maps both `egress.test` and `denied.test` to the same test IP, boots a VM
   behind the tap, then via the guest agent (`test-agent --mode name-egress`)
   asserts: an un-resolved direct dial to the IP is blocked; resolve+connect to
   `egress.test:8080` succeeds; `egress.test:9090` (wrong port) is blocked; and
   `denied.test` is refused by the resolver (so it is unreachable even though it
   maps to the same IP), proving allowlisting is by name, not by IP.
