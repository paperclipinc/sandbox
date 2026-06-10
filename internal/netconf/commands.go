package netconf

import (
	"fmt"
	"net"
)

// This file holds pure argv builders for the host networking commands. No
// exec happens here so the argument shapes are unit testable on any platform;
// the Linux-tagged internal/network package feeds these to a real runner.

// TapAddArgs builds the argv to create a tap device:
// ip tuntap add <tap> mode tap.
func TapAddArgs(tap string) []string {
	return []string{"ip", "tuntap", "add", tap, "mode", "tap"}
}

// AddrAddArgs builds the argv to assign the host side of the per-sandbox /30
// to the tap: ip addr add <hostIP>/30 dev <tap>.
func AddrAddArgs(hostIP net.IP, tap string) []string {
	return []string{"ip", "addr", "add", fmt.Sprintf("%s/30", hostIP.String()), "dev", tap}
}

// LinkUpArgs builds the argv to bring the tap up: ip link set <tap> up.
func LinkUpArgs(tap string) []string {
	return []string{"ip", "link", "set", tap, "up"}
}

// LinkDelArgs builds the argv to remove the tap: ip link del <tap>.
func LinkDelArgs(tap string) []string {
	return []string{"ip", "link", "del", tap}
}

// NftApplyArgs builds the argv to apply a rendered ruleset from stdin:
// nft -f -. The caller pipes a rendered ruleset (RenderSharedTable or
// RenderSandboxChain) on stdin.
func NftApplyArgs() []string {
	return []string{"nft", "-f", "-"}
}

// NftDeleteDispatchElementArgs builds the argv to remove this tap's dispatch
// element from the shared verdict map: nft delete element inet <table> <map>
// { "<tap>" }. After this no traffic jumps into the sandbox chain, so the
// chain can be removed. Deleting by key needs no rule handle.
func NftDeleteDispatchElementArgs(tap string) []string {
	return []string{"nft", "delete", "element", "inet", SharedTableName(), DispatchMapName(),
		fmt.Sprintf("{ %q }", tap)}
}

// NftDeleteSandboxChainArgs builds the argv to remove this sandbox's regular
// chain from the shared table: nft delete chain inet <table> sb_<tap>. The
// shared table, base chain, and map are left intact for other sandboxes.
func NftDeleteSandboxChainArgs(tap string) []string {
	return []string{"nft", "delete", "chain", "inet", SharedTableName(), SandboxChainName(tap)}
}

// MasqueradeAddArgs builds the argv to add a MASQUERADE rule for the sandbox
// subnet on the uplink interface. This is optional (the node may already NAT
// the subnet); callers gate it behind a flag.
func MasqueradeAddArgs(subnetCIDR, uplink string) []string {
	return []string{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnetCIDR, "-o", uplink, "-j", "MASQUERADE"}
}

// MasqueradeDelArgs builds the argv to remove the MASQUERADE rule added by
// MasqueradeAddArgs.
func MasqueradeDelArgs(subnetCIDR, uplink string) []string {
	return []string{"iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnetCIDR, "-o", uplink, "-j", "MASQUERADE"}
}
