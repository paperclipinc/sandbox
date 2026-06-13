package husk

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/vsock"
)

func TestActivateRequestRoundTrip(t *testing.T) {
	want := ActivateRequest{
		SnapshotDir: "/data/templates/tmpl-a/snapshot",
		NetworkOverrides: []firecracker.NetworkOverride{
			{IfaceID: "eth0", HostDevName: "tap-fork-1"},
		},
		Env:     map[string]string{"PATH": "/usr/bin", "LANG": "C"},
		Secrets: map[string]string{"API_KEY": "s3cr3t-value"},
		Network: &vsock.NotifyForkedNetwork{
			GuestIP:   "10.0.0.2",
			GatewayIP: "10.0.0.1",
			PrefixLen: 30,
		},
		Volumes: []vsock.VolumeMountEntry{
			{Device: "/dev/vdb", MountPath: "/data"},
		},
	}

	var buf bytes.Buffer
	if err := WriteRequest(&buf, want); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Fatalf("WriteRequest did not newline-terminate: %q", buf.String())
	}

	got, err := ReadRequest(&buf)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestActivateResultRoundTrip(t *testing.T) {
	want := ActivateResult{
		OK:        true,
		VsockPath: "/run/husk/vsock.sock",
		LatencyMs: 4.275,
	}

	var buf bytes.Buffer
	if err := WriteResult(&buf, want); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}

	got, err := ReadResult(&buf)
	if err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestActivateResultErrorRoundTrip(t *testing.T) {
	want := ActivateResult{OK: false, Error: "load snapshot: boom"}

	var buf bytes.Buffer
	if err := WriteResult(&buf, want); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	got, err := ReadResult(&buf)
	if err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	if got.OK || got.Error != want.Error {
		t.Fatalf("error result mismatch: got %+v want %+v", got, want)
	}
}
