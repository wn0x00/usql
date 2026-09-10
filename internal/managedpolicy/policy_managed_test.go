//go:build managed

package managedpolicy

import "testing"

func TestManagedPolicyBlocksDangerousCommands(t *testing.T) {
	for _, test := range []struct {
		name   string
		params string
	}{
		{"!", "whoami"},
		{"copy", "source target query table"},
		{"e", ""},
		{"getenv", "SECRET TOKEN"},
		{"i", "script.sql"},
		{"o", "output.txt"},
		{"setenv", "PAGER cmd"},
		{"g", "|whoami"},
	} {
		if err := CheckMetaCommand(test.name, test.params); err == nil {
			t.Fatalf("expected \\%s to be blocked", test.name)
		}
	}
}

func TestManagedPolicyAllowsInteractiveSQLHelpers(t *testing.T) {
	for _, name := range []string{"q", "?", "c", "conninfo", "g", "p", "r", "bind", "timing"} {
		if err := CheckMetaCommand(name, ""); err != nil {
			t.Fatalf("expected \\%s to be allowed: %v", name, err)
		}
	}
}

func TestManagedPolicyOnlyAllowsIPassDriver(t *testing.T) {
	if err := CheckIPassDriver("ipass"); err != nil {
		t.Fatal(err)
	}
	if err := CheckIPassDriver("postgres"); err == nil {
		t.Fatal("expected direct postgres driver to be blocked")
	}
}
