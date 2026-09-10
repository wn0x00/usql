// Package managedpolicy contains the compile-time security policy used by the
// Yingdao managed build. Community builds retain the upstream behaviour.
package managedpolicy

import (
	"fmt"
	"strings"
)

// safeMetaCommands deliberately stays small. SQL terminated by a semicolon is
// handled outside of the meta-command dispatcher and remains available.
var safeMetaCommands = map[string]bool{
	"?":          true,
	"Z":          true,
	"bind":       true,
	"c":          true,
	"connect":    true,
	"conninfo":   true,
	"copyright":  true,
	"disconnect": true,
	"drivers":    true,
	"ego":        true,
	"exec":       true,
	"g":          true,
	"G":          true,
	"go":         true,
	"gx":         true,
	"p":          true,
	"print":      true,
	"q":          true,
	"quit":       true,
	"r":          true,
	"raw":        true,
	"reset":      true,
	"timing":     true,
}

// MetaCommandAllowed reports whether a meta-command is available in a managed
// build. Forced-execution commands are accepted only without parameters so
// their FILE and |pipe forms cannot reach the local filesystem or shell.
func MetaCommandAllowed(name, rawParams string) bool {
	if !Enabled {
		return true
	}
	if !safeMetaCommands[name] {
		return false
	}
	switch name {
	case "g", "G", "go", "ego", "gx":
		return strings.TrimSpace(rawParams) == ""
	default:
		return true
	}
}

// CheckMetaCommand returns a stable error for a command blocked by the managed
// build.
func CheckMetaCommand(name, rawParams string) error {
	if MetaCommandAllowed(name, rawParams) {
		return nil
	}
	return fmt.Errorf("meta-command \\%s is disabled in the managed edition", name)
}

// CheckIPassDriver rejects every direct database driver in a managed build.
func CheckIPassDriver(driver string) error {
	if !Enabled || driver == "ipass" {
		return nil
	}
	return fmt.Errorf("database driver %q is disabled in the managed edition; use ipass://ALIAS", driver)
}

// FeatureDisabled returns a consistent error for command-line features which
// would read from or write to the sandbox filesystem, start a transaction, or
// load user-controlled configuration.
func FeatureDisabled(feature string) error {
	return fmt.Errorf("%s is disabled in the managed edition", feature)
}
