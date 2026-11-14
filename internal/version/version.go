// Package version is the version of harness that is running.
package version

import "runtime/debug"

// set is written by a release build: -ldflags "-X .../internal/version.set=v0.4.0".
var set string

// Get returns the harness's version: the release's, or the module version `go
// install` recorded, or "dev" for a build from a checkout.
func Get() string {
	if set != "" {
		return set
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
