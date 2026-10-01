package localcommit

import (
	"os/exec"
	"strings"
	"testing"
)

func TestShowCommit(t *testing.T) {
	t.Run("shows commit subject and patch", func(t *testing.T) {
		tmpDir := t.TempDir()

		cmds := [][]string{
			{"git", "init", "-b", "main"},
			{"git", "config", "user.email", "test@example.com"},
			{"git", "config", "user.name", "Test User"},
			{"git", "commit", "--allow-empty", "-m", "initial commit"},
		}

		for _, args := range cmds {
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Dir = tmpDir
			if err := cmd.Run(); err != nil {
				t.Fatalf("setup failed: %v", err)
			}
		}

		// Create a test commit
		testFile := tmpDir + "/test.txt"
		if err := exec.Command("sh", "-c", "echo 'test content' > "+testFile).Run(); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}

		cmd := exec.Command("git", "-C", tmpDir, "add", "test.txt")
		if err := cmd.Run(); err != nil {
			t.Fatalf("git add failed: %v", err)
		}

		cmd = exec.Command("git", "-C", tmpDir, "commit", "-m", "add test file")
		if err := cmd.Run(); err != nil {
			t.Fatalf("git commit failed: %v", err)
		}

		// Get the commit SHA
		cmd = exec.Command("git", "-C", tmpDir, "rev-parse", "HEAD")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git rev-parse failed: %v", err)
		}
		sha := strings.TrimSpace(string(out))

		// Test ShowCommit
		result, err := ShowCommit(tmpDir, sha)
		if err != nil {
			t.Errorf("ShowCommit() error = %v", err)
		}

		if !strings.Contains(result, "add test file") {
			t.Errorf("ShowCommit() did not contain commit subject 'add test file'")
		}

		if !strings.Contains(result, "test content") {
			t.Errorf("ShowCommit() did not contain patch content 'test content'")
		}
	})

	t.Run("unknown SHA returns error", func(t *testing.T) {
		tmpDir := t.TempDir()

		cmd := exec.Command("git", "init", "-b", "main")
		cmd.Dir = tmpDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		_, err := ShowCommit(tmpDir, "0000000000000000000000000000000000000000")
		if err == nil {
			t.Errorf("ShowCommit() with unknown SHA should return error")
		}
	})
}

func TestDiffBase(t *testing.T) {
	t.Run("shows delta vs base", func(t *testing.T) {
		tmpDir := t.TempDir()

		cmds := [][]string{
			{"git", "init", "-b", "main"},
			{"git", "config", "user.email", "test@example.com"},
			{"git", "config", "user.name", "Test User"},
			{"git", "commit", "--allow-empty", "-m", "initial"},
		}

		for _, args := range cmds {
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Dir = tmpDir
			if err := cmd.Run(); err != nil {
				t.Fatalf("setup failed: %v", err)
			}
		}

		// Set up origin/main as a ref to current HEAD
		cmd := exec.Command("git", "-C", tmpDir, "update-ref", "refs/remotes/origin/main", "HEAD")
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to set origin/main: %v", err)
		}

		// Create a test file on main
		testFile := tmpDir + "/base.txt"
		if err := exec.Command("sh", "-c", "echo 'base content' > "+testFile).Run(); err != nil {
			t.Fatalf("failed to create base file: %v", err)
		}

		cmd = exec.Command("git", "-C", tmpDir, "add", "base.txt")
		if err := cmd.Run(); err != nil {
			t.Fatalf("git add failed: %v", err)
		}

		cmd = exec.Command("git", "-C", tmpDir, "commit", "-m", "add base file")
		if err := cmd.Run(); err != nil {
			t.Fatalf("git commit failed: %v", err)
		}

		// Update origin/main to this commit
		cmd = exec.Command("git", "-C", tmpDir, "update-ref", "refs/remotes/origin/main", "HEAD")
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to update origin/main: %v", err)
		}

		// Create a new commit
		newFile := tmpDir + "/new.txt"
		if err := exec.Command("sh", "-c", "echo 'new content' > "+newFile).Run(); err != nil {
			t.Fatalf("failed to create new file: %v", err)
		}

		cmd = exec.Command("git", "-C", tmpDir, "add", "new.txt")
		if err := cmd.Run(); err != nil {
			t.Fatalf("git add failed: %v", err)
		}

		cmd = exec.Command("git", "-C", tmpDir, "commit", "-m", "add new file")
		if err := cmd.Run(); err != nil {
			t.Fatalf("git commit failed: %v", err)
		}

		// Get the commit SHA
		cmd = exec.Command("git", "-C", tmpDir, "rev-parse", "HEAD")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git rev-parse failed: %v", err)
		}
		sha := strings.TrimSpace(string(out))

		// Test DiffBase
		result, err := DiffBase(tmpDir, "item", sha)
		if err != nil {
			t.Errorf("DiffBase() error = %v", err)
		}

		if !strings.Contains(result, "new.txt") {
			t.Errorf("DiffBase() did not contain new.txt")
		}

		if !strings.Contains(result, "new content") {
			t.Errorf("DiffBase() did not contain 'new content'")
		}
	})

	t.Run("unknown SHA returns error", func(t *testing.T) {
		tmpDir := t.TempDir()

		cmd := exec.Command("git", "init", "-b", "main")
		cmd.Dir = tmpDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		_, err := DiffBase(tmpDir, "item", "0000000000000000000000000000000000000000")
		if err == nil {
			t.Errorf("DiffBase() with unknown SHA should return error")
		}
	})
}
