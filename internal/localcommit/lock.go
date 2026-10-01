package localcommit

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// lockRefPrefix is where branch locks live: refs/odonian/locks/<slug> guards wi/<slug>. Refs
// outside refs/heads are shared by every worktree of the repository and are not branches, so
// nothing checks them out or pushes them by accident.
const lockRefPrefix = "refs/odonian/locks/"

// lockMargin is how much longer than the merge gate an approve holds its branch lock, covering
// the git work and the done transition around the gate.
const lockMargin = 5 * time.Minute

// branchLockOwner is the content of a lock: who holds it and until when. It is stored as a blob
// the lock ref points at, so taking, stealing, and releasing the lock are all compare-and-swap
// updates of that one ref.
type branchLockOwner struct {
	Host    string    `json:"host"`
	PID     int       `json:"pid"`
	Since   time.Time `json:"since"`
	Expires time.Time `json:"expires"`
	Nonce   string    `json:"nonce"`
}

// BranchLock is a held approve lock on one MR branch. Publish takes it to prove, atomically with
// moving the branch, that this approve still holds the lock.
type BranchLock struct {
	repoDir string
	ref     string
	owner   string // the object the lock ref points at while this approve holds it
}

// ErrLockLost is returned (wrapped) by Publish when this approve's lock expired and was taken
// over: it published nothing, and anything else it does now may undo the new holder's work.
var ErrLockLost = errors.New("branch lock lost")

// Token uniquely identifies this acquisition of the lock. approve uses it as its landing attempt
// id, so a reservation records which approve owns it.
func (lock *BranchLock) Token() string {
	return lock.owner
}

// Release gives the lock back, if it is still this approve's.
func (lock *BranchLock) Release() {
	_, _ = git(lock.repoDir, "update-ref", "-d", lock.ref, lock.owner)
}

// LockBranch takes the exclusive approve lock on wi/<slug> for at most holdFor. An approve holds
// it from before it prepares a task's landing until after it has published it, so no other
// approve can move wi/<slug> in between.
//
// The lock is a git ref updated with compare-and-swap, the same primitive git itself uses for
// branches, so it works wherever git does, including a repository shared between the host and a
// sandbox over a bind mount (where flock is unreliable). A lock past its expiry belongs to a
// crashed approve and is taken over; the approve that held it can then no longer publish (see
// Publish), so a stalled approve cannot act on a decision made after its lock was taken over.
func LockBranch(repoDir, slug string, holdFor time.Duration) (*BranchLock, error) {
	lockRef := lockRefPrefix + slug
	hostname, _ := os.Hostname()
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	now := time.Now().UTC()
	owner := branchLockOwner{Host: hostname, PID: os.Getpid(), Since: now, Expires: now.Add(holdFor), Nonce: hex.EncodeToString(nonce)}
	payload, err := json.Marshal(owner)
	if err != nil {
		return nil, fmt.Errorf("failed to encode branch lock: %w", err)
	}
	ownerObject, err := gitWithInput(repoDir, payload, "hash-object", "-w", "--stdin")
	if err != nil {
		return nil, fmt.Errorf("failed to write branch lock: %w", err)
	}

	// Two attempts: the lock can change hands between reading it and swapping it.
	for attempt := 0; attempt < 2; attempt++ {
		expectedObject := "" // the lock ref must not exist
		if heldObject, err := git(repoDir, "rev-parse", "--verify", "--quiet", lockRef); err == nil {
			holder, err := readBranchLockOwner(repoDir, heldObject)
			if err != nil {
				return nil, fmt.Errorf("the approve lock on %s is unreadable (%v); if no approve is running, remove it with 'git update-ref -d %s' and re-run", MRBranch(slug), err, lockRef)
			}
			if time.Now().Before(holder.Expires) {
				return nil, fmt.Errorf("another approve is landing work on %s (pid %d on %s, since %s; its lock expires %s); wait for it to finish, then re-run",
					MRBranch(slug), holder.PID, holder.Host, holder.Since.Format(time.RFC3339), holder.Expires.Format(time.RFC3339))
			}
			expectedObject = heldObject // expired: a crashed approve's lock, take it over
		}
		if _, err := git(repoDir, "update-ref", lockRef, ownerObject, expectedObject); err == nil {
			return &BranchLock{repoDir: repoDir, ref: lockRef, owner: ownerObject}, nil
		}
	}
	return nil, fmt.Errorf("could not take the approve lock on %s: another approve took it at the same moment; wait for it to finish, then re-run", MRBranch(slug))
}

func readBranchLockOwner(repoDir, object string) (branchLockOwner, error) {
	content, err := git(repoDir, "cat-file", "blob", object)
	if err != nil {
		return branchLockOwner{}, err
	}
	var owner branchLockOwner
	if err := json.Unmarshal([]byte(content), &owner); err != nil {
		return branchLockOwner{}, err
	}
	return owner, nil
}

// gitWithInput is git with input on stdin.
func gitWithInput(dir string, input []byte, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
