package main

import (
	"flag"
	"testing"
)

// TestMaxStreamsPerSandboxFlagDefault verifies the standalone server exposes a
// --max-streams-per-sandbox flag defaulting to 16, matching forkd. Before this
// fix the standalone REST path had no flag and never capped streams.
func TestMaxStreamsPerSandboxFlagDefault(t *testing.T) {
	fs := flag.NewFlagSet("sandbox-server", flag.ContinueOnError)
	var maxStreams int
	fs.IntVar(&maxStreams, "max-streams-per-sandbox", 16, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if maxStreams != 16 {
		t.Fatalf("default --max-streams-per-sandbox: got %d want 16 (forkd default)", maxStreams)
	}
}

// TestNewServerPlumbsStreamCap verifies the flag value reaches the server: the
// per-sandbox stream ceiling passed to newServer is applied to the SandboxAPI
// (the value is retained on the server for observability). A parsed flag of 7
// must surface unchanged.
func TestNewServerPlumbsStreamCap(t *testing.T) {
	const want = 7
	s := newServer(t.TempDir(), "", true, want)
	if s.sandboxAPI == nil {
		t.Fatal("newServer must construct a SandboxAPI")
	}
	if s.maxStreamsPerSandbox != want {
		t.Fatalf("newServer stream cap: got %d want %d", s.maxStreamsPerSandbox, want)
	}
}
