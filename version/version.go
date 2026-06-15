// Package version holds the single source of truth for the build version,
// injected at build time via ldflags:
//
//	go build -ldflags "-X 'github.com/usesalvager/salvager/version.Version=1.0.0'" .
//
// A build with no ldflags reports "dev" so it is never mistaken for a release.
package version

// Version is the binary's version. Overridden at build time via -X; "dev" otherwise.
var Version = "dev"
