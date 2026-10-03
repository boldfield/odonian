package evaluation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *testRepo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

func byPath(files []WorkspaceFile) map[string]string {
	m := map[string]string{}
	for _, f := range files {
		m[f.Path] = string(f.Content)
	}
	return m
}

func TestGitSourceExportsTheOriginalCommitNotTheFix(t *testing.T) {
	r := newTestRepo(t)
	r.write("README.md", "base\n")
	r.write("notes/keep.md", "unchanged context\n")
	base := r.commit("base")
	r.write("research/claims.md", "Claim 1: the court held X. [FLAW-ORIGINAL]\n")
	submitted := r.commit("round 1 submission")
	r.write("research/claims.md", "Claim 1: the court held Y. [FIXED-LATER]\n")
	r.write("research/extra.md", "added in the fix\n")
	fixed := r.commit("address reviewer feedback: reviewer says claim 1 is wrong")
	r.git("update-ref", "refs/heads/main", fixed)
	r.write("research/claims.md", "uncommitted working tree edit\n")

	src := GitSource{Dir: r.dir}
	ctx := context.Background()

	files, err := src.ChangedFiles(ctx, submitted)
	if err != nil {
		t.Fatal(err)
	}
	got := byPath(files)
	if len(got) != 1 || got["research/claims.md"] != "Claim 1: the court held X. [FLAW-ORIGINAL]\n" {
		t.Fatalf("artifact is not the rejected-before-fix snapshot: %v", got)
	}
	for _, f := range files {
		if strings.Contains(string(f.Content), "FIXED-LATER") {
			t.Fatal("late fix reached the export")
		}
	}

	ctxFiles, err := src.TreeFiles(ctx, submitted, []string{"notes"})
	if err != nil {
		t.Fatal(err)
	}
	if g := byPath(ctxFiles); len(g) != 1 || g["notes/keep.md"] == "" {
		t.Fatalf("context files = %v", g)
	}
	if _, err := src.TreeFiles(ctx, submitted, []string{"../etc"}); !errors.Is(err, ErrUnsafeWorkspace) {
		t.Fatalf("traversal in context path: %v", err)
	}

	root, err := src.ChangedFiles(ctx, base)
	if err != nil || len(byPath(root)) != 2 {
		t.Fatalf("root commit export = %v, %v", byPath(root), err)
	}
}

func TestGitSourceMissingAndNonCommitObjects(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.md", "a\n")
	sha := r.commit("one")
	blob := r.git("rev-parse", "HEAD:a.md")
	src := GitSource{Dir: r.dir}
	ctx := context.Background()

	for name, id := range map[string]string{
		"absent":        strings.Repeat("0", 40),
		"a blob":        blob,
		"branch name":   "main",
		"abbreviated":   sha[:10],
		"revision expr": sha + "~1",
		"empty":         "",
	} {
		if err := src.CommitExists(ctx, id); !errors.Is(err, ErrCommitUnavailable) {
			t.Errorf("%s: CommitExists err = %v", name, err)
		}
		if _, err := src.ChangedFiles(ctx, id); !errors.Is(err, ErrCommitUnavailable) {
			t.Errorf("%s: ChangedFiles err = %v", name, err)
		}
	}
	if err := src.CommitExists(ctx, sha); err != nil {
		t.Fatalf("real commit: %v", err)
	}
	if err := (GitSource{Dir: filepath.Join(r.dir, "nope")}).CommitExists(ctx, sha); !errors.Is(err, ErrCommitUnavailable) {
		t.Fatalf("missing repository: %v", err)
	}
}

func TestGitSourceIgnoresReplaceRefsAndAttributes(t *testing.T) {
	r := newTestRepo(t)
	r.write(".gitattributes", "research/*.md export-subst ident\n")
	r.write("research/a.md", "$Format:%H$ $Id$ original\n")
	sha := r.commit("submission")
	cmd := exec.Command("git", "-C", r.dir, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader("REPLACED CONTENT\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	r.git("replace", r.git("rev-parse", "HEAD:research/a.md"), strings.TrimSpace(string(out)))

	files, err := GitSource{Dir: r.dir}.ChangedFiles(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	if got := byPath(files)["research/a.md"]; got != "$Format:%H$ $Id$ original\n" {
		t.Fatalf("replace ref or attribute substitution changed the export: %q", got)
	}
}

func TestGitSourceRefusesSymlinksAndSubmodules(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.md", "a\n")
	r.commit("base")
	if err := os.Symlink("/etc/passwd", filepath.Join(r.dir, "link.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	sha := r.commit("adds a symlink")
	if _, err := (GitSource{Dir: r.dir}).ChangedFiles(context.Background(), sha); !errors.Is(err, ErrUnsupportedEntry) {
		t.Fatalf("symlink exported: %v", err)
	}
}
