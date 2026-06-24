// Package version holds the build version of the frostgate/frostcode binaries.
// Version is overridden at build time with -ldflags "-X
// frostgate/internal/version.Version=v1.2.3"; it defaults to "dev" for local
// builds. The release workflow stamps the git tag here, and /update compares
// against it to decide whether a newer GitHub release is available.
package version

// Version is the current build version (a semver tag like "v0.2.0", or "dev").
var Version = "dev"
