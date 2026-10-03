// Command muse-adapter is the Muse Code comparison-reviewer adapter. See
// docs/runbooks/muse-code-adapter.md.
package main

import (
	"os"

	"github.com/boldfield/odonian/internal/evaluation"
)

func main() {
	os.Exit(evaluation.MuseMain(os.Args[1:], os.Stdout, os.Stderr))
}
