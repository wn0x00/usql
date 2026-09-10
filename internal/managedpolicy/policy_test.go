package managedpolicy

import "testing"

func TestCommunityPolicyDoesNotBlockCommands(t *testing.T) {
	if Enabled {
		t.Skip("community-build assertion")
	}
	if err := CheckMetaCommand("!", "whoami"); err != nil {
		t.Fatalf("community build unexpectedly blocked command: %v", err)
	}
}
