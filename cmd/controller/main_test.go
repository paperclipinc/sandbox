package main

import "testing"

// TestResolveRunMode pins the single-active-path wiring: husk pods is the
// pod-native default, --enable-raw-forkd is the fallback, and --mock forces
// raw-forkd (the dev mock overlay has no KVM, so husk pods cannot run there).
func TestResolveRunMode(t *testing.T) {
	cases := []struct {
		name           string
		enableHuskPods bool // the flag default (true)
		enableRawForkd bool
		mockMode       bool
		wantHusk       bool
		wantRaw        bool
	}{
		{name: "default is husk", enableHuskPods: true, wantHusk: true, wantRaw: false},
		{name: "raw-forkd flag selects raw", enableHuskPods: true, enableRawForkd: true, wantHusk: false, wantRaw: true},
		{name: "mock forces raw", enableHuskPods: true, mockMode: true, wantHusk: false, wantRaw: true},
		{name: "mock plus raw flag is raw", enableHuskPods: true, enableRawForkd: true, mockMode: true, wantHusk: false, wantRaw: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			husk, raw := resolveRunMode(tc.enableHuskPods, tc.enableRawForkd, tc.mockMode)
			if husk != tc.wantHusk || raw != tc.wantRaw {
				t.Fatalf("resolveRunMode(%v,%v,%v) = (husk %v, raw %v), want (husk %v, raw %v)",
					tc.enableHuskPods, tc.enableRawForkd, tc.mockMode, husk, raw, tc.wantHusk, tc.wantRaw)
			}
			// Exactly one path is active.
			if husk == raw {
				t.Errorf("husk and raw must be mutually exclusive, got husk=%v raw=%v", husk, raw)
			}
		})
	}
}
