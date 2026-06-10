package netconf

import (
	"net"
	"reflect"
	"testing"
)

func TestTapAddArgs(t *testing.T) {
	got := TapAddArgs("sbtap0")
	want := []string{"ip", "tuntap", "add", "sbtap0", "mode", "tap"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TapAddArgs = %v, want %v", got, want)
	}
}

func TestAddrAddArgs(t *testing.T) {
	got := AddrAddArgs(net.ParseIP("10.200.0.1"), "sbtap0")
	want := []string{"ip", "addr", "add", "10.200.0.1/30", "dev", "sbtap0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AddrAddArgs = %v, want %v", got, want)
	}
}

func TestLinkUpArgs(t *testing.T) {
	got := LinkUpArgs("sbtap0")
	want := []string{"ip", "link", "set", "sbtap0", "up"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LinkUpArgs = %v, want %v", got, want)
	}
}

func TestLinkDelArgs(t *testing.T) {
	got := LinkDelArgs("sbtap0")
	want := []string{"ip", "link", "del", "sbtap0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LinkDelArgs = %v, want %v", got, want)
	}
}

func TestNftApplyArgs(t *testing.T) {
	got := NftApplyArgs()
	want := []string{"nft", "-f", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NftApplyArgs = %v, want %v", got, want)
	}
}

func TestNftDeleteTableArgs(t *testing.T) {
	got := NftDeleteTableArgs("sbtap0")
	want := []string{"nft", "delete", "table", "ip", TableName("sbtap0")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NftDeleteTableArgs = %v, want %v", got, want)
	}
}

func TestMasqueradeAddArgs(t *testing.T) {
	got := MasqueradeAddArgs("10.200.0.0/16", "eth0")
	want := []string{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "10.200.0.0/16", "-o", "eth0", "-j", "MASQUERADE"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MasqueradeAddArgs = %v, want %v", got, want)
	}
}

func TestMasqueradeDelArgs(t *testing.T) {
	got := MasqueradeDelArgs("10.200.0.0/16", "eth0")
	want := []string{"iptables", "-t", "nat", "-D", "POSTROUTING", "-s", "10.200.0.0/16", "-o", "eth0", "-j", "MASQUERADE"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MasqueradeDelArgs = %v, want %v", got, want)
	}
}
