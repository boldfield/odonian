//go:build !unix

package localcommit

import "os/exec"

// killProcessGroupOnCancel falls back to exec's default cancellation (kill the process itself)
// where process groups are unavailable.
func killProcessGroupOnCancel(cmd *exec.Cmd) {}
