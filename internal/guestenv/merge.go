// Package guestenv builds the environment for guest exec sessions.
// Kept separate from the linux-only guest agent so the precedence rules are
// unit-testable on any platform.
package guestenv

import "strings"

// Merge combines a base environment (os.Environ format) with configured
// (claim-time env+secrets) and per-request variables. Precedence, lowest to
// highest: base < configured < request. Later duplicates win.
func Merge(base []string, configured, request map[string]string) []string {
	merged := make(map[string]string, len(base)+len(configured)+len(request))
	order := make([]string, 0, len(base)+len(configured)+len(request))

	set := func(k, v string) {
		if _, seen := merged[k]; !seen {
			order = append(order, k)
		}
		merged[k] = v
	}

	// Base entries without '=' have no key to merge on; pass them through
	// verbatim rather than silently dropping them.
	var verbatim []string
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		} else {
			verbatim = append(verbatim, kv)
		}
	}
	for k, v := range configured {
		set(k, v)
	}
	for k, v := range request {
		set(k, v)
	}

	out := make([]string, 0, len(verbatim)+len(order))
	out = append(out, verbatim...)
	for _, k := range order {
		out = append(out, k+"="+merged[k])
	}
	return out
}
