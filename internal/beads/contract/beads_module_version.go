package contract

import (
	"runtime/debug"
	"strings"
)

// BeadsModuleVersion returns the github.com/steveyegge/beads module version
// recorded in info, or "" when info does not carry beads. It reads both
// shapes a binary can take: beads as the main module (a `go install`ed bd)
// and beads as a dependency (gc, or a bd built from a scratch module). A
// replaced dependency reports the replacement's own version, as
// linkedBeadsLibraryFrom does.
func BeadsModuleVersion(info *debug.BuildInfo) string {
	if info == nil {
		return ""
	}
	if info.Main.Path == beadsModulePath {
		return strings.TrimSpace(info.Main.Version)
	}
	return strings.TrimSpace(linkedBeadsLibraryFrom(info).Version)
}
