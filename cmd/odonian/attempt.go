package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/boldfield/odonian/internal/tuiclient"
)

// A research claim returns an attempt ID that heartbeat and submit must send back
// so a superseded attempt is fenced. The claim, heartbeat and submit are separate
// CLI processes, so the claim saves the ID in a small per-task file that the
// later commands read. --attempt overrides it; a task with no file sends none.

var safeTaskID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func attemptDir() string {
	if d := os.Getenv("ODONIAN_STATE_DIR"); d != "" {
		return filepath.Join(d, "attempts")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "odonian", "attempts")
	}
	return filepath.Join(os.TempDir(), "odonian-attempts")
}

func attemptPath(taskID string) (string, bool) {
	if !safeTaskID.MatchString(taskID) {
		return "", false
	}
	return filepath.Join(attemptDir(), taskID), true
}

// saveAttempt records the attempt the client's claim returned for taskID, or
// clears a previous one when the claim carried none. Failures are ignored: the
// attempt fence is an addition, and a claim must not fail for lack of a cache dir.
func saveAttempt(client *tuiclient.HTTPClient, taskID string) {
	path, ok := attemptPath(taskID)
	if !ok {
		return
	}
	id := client.AttemptID(taskID)
	if id == "" {
		os.Remove(path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	os.WriteFile(path, []byte(id+"\n"), 0o600)
}

// loadAttempt points the client at the attempt for taskID: override if given,
// else the one saved by the claim.
func loadAttempt(client *tuiclient.HTTPClient, taskID, override string) {
	if override != "" {
		client.SetAttemptID(taskID, override)
		return
	}
	path, ok := attemptPath(taskID)
	if !ok {
		return
	}
	if b, err := os.ReadFile(path); err == nil {
		client.SetAttemptID(taskID, strings.TrimSpace(string(b)))
	}
}

func clearAttempt(taskID string) {
	if path, ok := attemptPath(taskID); ok {
		os.Remove(path)
	}
}
