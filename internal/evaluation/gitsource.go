package evaluation

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ErrCommitUnavailable means the pinned commit cannot be read from the
// repository: it is absent, unreachable, or not a commit.
var ErrCommitUnavailable = errors.New("pinned commit is unavailable")

// ErrUnsupportedEntry means the commit contains an entry (a symlink or
// submodule) that cannot be exported as a plain file.
var ErrUnsupportedEntry = errors.New("unsupported tree entry")

// CommitSource exports files as they were at one exact commit. It must never
// resolve a branch, tag or the current tree.
type CommitSource interface {
	// CommitExists returns ErrCommitUnavailable (wrapped) if sha is not a commit
	// in the repository.
	CommitExists(ctx context.Context, sha string) error
	// ChangedFiles returns the files the commit added or modified relative to
	// its first parent (every file for a root commit), at their content in the
	// commit.
	ChangedFiles(ctx context.Context, sha string) ([]WorkspaceFile, error)
	// TreeFiles returns files from the commit's tree whose path equals, or is
	// below, one of paths.
	TreeFiles(ctx context.Context, sha string, paths []string) ([]WorkspaceFile, error)
}

// GitSource reads a local git repository. Export goes through ls-tree,
// diff-tree and cat-file --batch, so no checkout, archive attribute
// substitution, filter, textconv, replace ref or hook is involved, and no
// history is copied.
type GitSource struct {
	Dir string
}

var _ CommitSource = GitSource{}

func (g GitSource) git(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.Dir, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = []string{
		"PATH=" + childPath,
		"HOME=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(truncate(stderr.String())))
	}
	return stdout.Bytes(), nil
}

func (g GitSource) CommitExists(ctx context.Context, sha string) error {
	if !commitSHARE.MatchString(sha) {
		return fmt.Errorf("%w: %q is not a full object id", ErrCommitUnavailable, sha)
	}
	if _, err := g.git(ctx, nil, "cat-file", "-e", sha+"^{commit}"); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %s", ErrCommitUnavailable, sha)
	}
	return nil
}

type treeEntry struct {
	mode string
	blob string
	path string
}

func parseLsTree(out []byte) ([]treeEntry, error) {
	var entries []treeEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, name, ok := bytes.Cut(rec, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("unparseable ls-tree record %q", rec)
		}
		entries = append(entries, treeEntry{mode: fields[0], blob: fields[2], path: string(name)})
	}
	return entries, nil
}

func parseDiffTree(out []byte) ([]treeEntry, error) {
	parts := bytes.Split(out, []byte{0})
	var entries []treeEntry
	for i := 0; i+1 < len(parts); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(string(parts[i]), ":"))
		if len(meta) != 5 {
			return nil, fmt.Errorf("unparseable diff-tree record %q", parts[i])
		}
		entries = append(entries, treeEntry{mode: meta[1], blob: meta[3], path: string(parts[i+1])})
	}
	return entries, nil
}

func (g GitSource) ChangedFiles(ctx context.Context, sha string) ([]WorkspaceFile, error) {
	if err := g.CommitExists(ctx, sha); err != nil {
		return nil, err
	}
	out, err := g.git(ctx, nil, "rev-list", "--parents", "-n", "1", sha)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	var entries []treeEntry
	if len(ids) < 2 {
		raw, err := g.git(ctx, nil, "ls-tree", "-r", "-z", "--full-tree", sha)
		if err != nil {
			return nil, err
		}
		if entries, err = parseLsTree(raw); err != nil {
			return nil, err
		}
	} else {
		raw, err := g.git(ctx, nil, "diff-tree", "-r", "-z", "--no-renames", "--no-textconv", "--no-ext-diff", "--diff-filter=AMT", ids[1], sha)
		if err != nil {
			return nil, err
		}
		if entries, err = parseDiffTree(raw); err != nil {
			return nil, err
		}
	}
	return g.export(ctx, entries)
}

func (g GitSource) TreeFiles(ctx context.Context, sha string, paths []string) ([]WorkspaceFile, error) {
	if err := g.CommitExists(ctx, sha); err != nil {
		return nil, err
	}
	for _, p := range paths {
		if err := ValidateWorkspacePath(p); err != nil {
			return nil, err
		}
	}
	raw, err := g.git(ctx, nil, "ls-tree", "-r", "-z", "--full-tree", sha)
	if err != nil {
		return nil, err
	}
	all, err := parseLsTree(raw)
	if err != nil {
		return nil, err
	}
	var entries []treeEntry
	for _, e := range all {
		for _, p := range paths {
			if e.path == p || strings.HasPrefix(e.path, p+"/") {
				entries = append(entries, e)
				break
			}
		}
	}
	return g.export(ctx, entries)
}

func (g GitSource) export(ctx context.Context, entries []treeEntry) ([]WorkspaceFile, error) {
	for _, e := range entries {
		if e.mode != "100644" && e.mode != "100755" {
			return nil, fmt.Errorf("%w: %s has mode %s", ErrUnsupportedEntry, e.path, e.mode)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	var req bytes.Buffer
	for _, e := range entries {
		req.WriteString(e.blob + "\n")
	}
	out, err := g.git(ctx, &req, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(bytes.NewReader(out))
	files := make([]WorkspaceFile, 0, len(entries))
	total := 0
	for _, e := range entries {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read blob header for %s: %w", e.path, err)
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[1] != "blob" {
			return nil, fmt.Errorf("%w: %s: %s", ErrCommitUnavailable, e.path, strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size < 0 {
			return nil, fmt.Errorf("bad blob size for %s", e.path)
		}
		if size > MaxWorkspaceFileBytes {
			return nil, fmt.Errorf("%w: %s is %d bytes", ErrWorkspaceTooLarge, e.path, size)
		}
		if total += size; total > MaxWorkspaceBytes {
			return nil, fmt.Errorf("%w: more than %d bytes", ErrWorkspaceTooLarge, MaxWorkspaceBytes)
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("read blob for %s: %w", e.path, err)
		}
		if _, err := r.ReadByte(); err != nil {
			return nil, fmt.Errorf("read blob terminator for %s: %w", e.path, err)
		}
		if err := ValidateWorkspacePath(e.path); err != nil {
			return nil, err
		}
		files = append(files, WorkspaceFile{Path: e.path, Executable: e.mode == "100755", Content: body})
	}
	return files, nil
}
