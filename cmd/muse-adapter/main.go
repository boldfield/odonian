// Command muse-adapter implements the Muse Code evaluation adapter.
// It reads a CandidateRequest from a JSON file, invokes the muse CLI,
// and writes a normalized CandidateResponse back to JSON.
package main

import (
	"os"

	"github.com/boldfield/odonian/internal/evaluation"
)

func main() {
	os.Exit(evaluation.MuseMain(os.Args[1:], os.Stderr))
}
