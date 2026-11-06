package main

import (
	"testing"

	"github.com/lucyna-aqu/mailbox-tunnel/tunnel"
)

func TestRouteOrder(t *testing.T) {
	google := tunnel.ClientRoute{Name: "Google"}
	relay := tunnel.ClientRoute{Name: "Relay"}
	cases := []struct {
		name                                     string
		hasRelay, relayUp, preferRelay, googleOK bool
		want                                     string
	}{
		{"no relay", false, false, false, true, "Google"},
		{"relay preferred and up", true, true, true, true, "Relay Google"},
		{"relay preferred but off, as when the codespace slept", true, false, true, true, "Google Relay"},
		{"relay up, Google broken", true, true, false, false, "Relay Google"},
		{"relay up, Google preferred and working", true, true, false, true, "Google Relay"},
		{"relay off, Google broken", true, false, false, false, "Google Relay"},
	}
	for _, c := range cases {
		got := ""
		for _, route := range routeOrder(google, relay, c.hasRelay, c.relayUp, c.preferRelay, c.googleOK) {
			if got != "" {
				got += " "
			}
			got += route.Name
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestScansGoogleUnlessAWorkingRelayIsPreferred(t *testing.T) {
	// The bug: with a relay saved, a stale Google route was never scanned for again, even when the relay was off.
	if !scansGoogle(false, true) {
		t.Error("a preferred relay that is off skipped the scan for Google")
	}
	if !scansGoogle(true, false) || !scansGoogle(false, false) {
		t.Error("Google preferred skipped the scan")
	}
	if scansGoogle(true, true) {
		t.Error("a working preferred relay waited for a scan of Google")
	}
}
