package main

import "testing"

func TestListenAddressPermitted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		address     string
		allowRemote string
		want        bool
	}{
		{"loopback by default", "127.0.0.1:18768", "", true},
		{"IPv6 loopback by default", "[::1]:18768", "", true},
		{"wildcard rejected", ":18768", "", false},
		{"all interfaces rejected", "0.0.0.0:18768", "", false},
		{"remote one is not opt-in", "10.0.0.8:18768", "1", false},
		{"remote yes is not opt-in", "10.0.0.8:18768", "yes", false},
		{"remote explicitly enabled", "10.0.0.8:18768", "true", true},
		{"wildcard explicitly enabled", ":18768", " TRUE ", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := listenAddressPermitted(test.address, test.allowRemote); got != test.want {
				t.Fatalf("listenAddressPermitted(%q, %q) = %v, want %v", test.address, test.allowRemote, got, test.want)
			}
		})
	}
}
