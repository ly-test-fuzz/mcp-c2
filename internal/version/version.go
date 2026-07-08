// Package version holds the build version string injected via ldflags
// (-X debugmcp/internal/version.Version=...). Set by scripts/build.sh from
// git describe / $VERSION and surfaced by every binary's -version flag.
package version

import (
	"flag"
	"fmt"
	"os"
)

// Version is overwritten at build time. Default "dev" when built without
// the ldflags injection (e.g. plain `go build ./...`).
var Version = "dev"

// RegisterFlag wires a -version bool flag into the given FlagSet that, when
// set, prints "<prog> v<version>" and exits 0. Pass flag.CommandLine for the
// default flag set. Centralizes the flag so every binary stays in sync.
// (Uses a bool flag — not flag.Func — so bare `-version` with no argument is
// accepted, matching CLI convention.)
func RegisterFlag(fs *flag.FlagSet, prog string) {
	fs.BoolFunc("version", "print build version and exit", func(_ string) error {
		fmt.Printf("%s v%s\n", prog, Version)
		os.Exit(0)
		return nil
	})
}

// HandleEarly checks os.Args for an exact -version / --version / -v token and,
// if present, prints "<prog> v<version>" and exits 0. For binaries that don't
// use the flag package (shim, probe) so they get a -version too without
// restructuring their arg parsing. Call it at the top of main().
func HandleEarly(prog string) {
	for _, a := range os.Args[1:] {
		switch a {
		case "-version", "--version", "-v":
			fmt.Printf("%s v%s\n", prog, Version)
			os.Exit(0)
		}
	}
}
