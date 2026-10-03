package evaluation

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Layout of a frozen review workspace. Only these three top-level names exist.
const (
	ArtifactDir    = "artifact"
	ContextDir     = "context"
	AcceptanceFile = "ACCEPTANCE.md"

	// BlindedPromptVersion identifies the prompt template.
	BlindedPromptVersion = "blinded-review-prompt/v1"

	MaxWorkspaceFiles     = 2000
	MaxWorkspaceFileBytes = 8 << 20
	MaxWorkspaceBytes     = 64 << 20

	// MinSealedLen is the shortest sealed string the leak scan will look for.
	// Shorter strings (a one-word finding summary) would match ordinary prose.
	MinSealedLen = 12
)

var (
	ErrWorkspaceExists   = errors.New("workspace already exists")
	ErrDigestMismatch    = errors.New("workspace digest mismatch")
	ErrUnsafeWorkspace   = errors.New("unsafe workspace content")
	ErrSealedContent     = errors.New("sealed content found in workspace or prompt")
	ErrWorkspaceTooLarge = errors.New("workspace exceeds its size limits")
)

// WorkspaceFile is one regular file to be staged.
type WorkspaceFile struct {
	Path       string // slash separated, relative to its section
	Executable bool
	Content    []byte
}

// WorkspaceSpec is everything a reviewer may see: the artifact, necessary
// source context, and the full acceptance criteria. Nothing else is staged.
type WorkspaceSpec struct {
	Artifact   []WorkspaceFile
	Context    []WorkspaceFile
	Acceptance string
}

// ValidateWorkspacePath rejects any path that could escape the workspace or
// smuggle in git metadata.
func ValidateWorkspacePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: empty path", ErrUnsafeWorkspace)
	case strings.ContainsAny(p, "\\\x00"):
		return fmt.Errorf("%w: path %q contains a backslash or NUL", ErrUnsafeWorkspace, p)
	case path.IsAbs(p) || path.Clean(p) != p:
		return fmt.Errorf("%w: path %q is absolute or not clean", ErrUnsafeWorkspace, p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: path %q contains a control character", ErrUnsafeWorkspace, p)
		}
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." || strings.EqualFold(part, ".git") {
			return fmt.Errorf("%w: path %q has a forbidden component %q", ErrUnsafeWorkspace, p, part)
		}
	}
	return nil
}

// stagedFile is a file at its final workspace-relative path.
type stagedFile struct {
	Path       string
	Executable bool
	Content    []byte
}

func (s WorkspaceSpec) staged() ([]stagedFile, error) {
	var out []stagedFile
	add := func(dir string, files []WorkspaceFile) error {
		for _, f := range files {
			if err := ValidateWorkspacePath(f.Path); err != nil {
				return err
			}
			out = append(out, stagedFile{Path: dir + "/" + f.Path, Executable: f.Executable, Content: f.Content})
		}
		return nil
	}
	if err := add(ArtifactDir, s.Artifact); err != nil {
		return nil, err
	}
	if err := add(ContextDir, s.Context); err != nil {
		return nil, err
	}
	if len(s.Artifact) == 0 {
		return nil, invalid("a workspace needs at least one artifact file")
	}
	if strings.TrimSpace(s.Acceptance) == "" {
		return nil, invalid("full acceptance criteria are required")
	}
	out = append(out, stagedFile{Path: AcceptanceFile, Content: []byte(s.Acceptance)})
	return out, checkStaged(out)
}

// checkStaged enforces uniqueness (case-insensitively, so the digest does not
// depend on the filesystem), no file/directory conflicts, and size limits.
func checkStaged(files []stagedFile) error {
	if len(files) > MaxWorkspaceFiles {
		return fmt.Errorf("%w: %d files", ErrWorkspaceTooLarge, len(files))
	}
	var total int
	seen := map[string]string{}
	dirs := map[string]bool{}
	for _, f := range files {
		if len(f.Content) > MaxWorkspaceFileBytes {
			return fmt.Errorf("%w: %s is %d bytes", ErrWorkspaceTooLarge, f.Path, len(f.Content))
		}
		total += len(f.Content)
		if total > MaxWorkspaceBytes {
			return fmt.Errorf("%w: more than %d bytes", ErrWorkspaceTooLarge, MaxWorkspaceBytes)
		}
		key := strings.ToLower(f.Path)
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("%w: %q collides with %q", ErrUnsafeWorkspace, f.Path, prev)
		}
		seen[key] = f.Path
		for d := path.Dir(key); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for key, p := range seen {
		if dirs[key] {
			return fmt.Errorf("%w: %q is both a file and a directory", ErrUnsafeWorkspace, p)
		}
	}
	return nil
}

func sortStaged(files []stagedFile) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
}

func digestStaged(files []stagedFile) string {
	files = append([]stagedFile(nil), files...)
	sortStaged(files)
	h := sha256.New()
	put := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	put([]byte("odonian-workspace/v1"))
	for _, f := range files {
		put([]byte(f.Path))
		mode := "644"
		if f.Executable {
			mode = "755"
		}
		put([]byte(mode))
		put(f.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func isContextFile(p string) bool { return strings.HasPrefix(p, ContextDir+"/") }

// Digests computes the snapshot digest over the whole workspace and the source
// digest over the source-context section alone. Both depend only on file
// paths, modes and bytes, never on map or directory iteration order.
func (s WorkspaceSpec) Digests() (snapshot, source string, err error) {
	files, err := s.staged()
	if err != nil {
		return "", "", err
	}
	return digestOf(files)
}

func digestOf(files []stagedFile) (snapshot, source string, err error) {
	var ctx []stagedFile
	for _, f := range files {
		if isContextFile(f.Path) {
			ctx = append(ctx, f)
		}
	}
	return digestStaged(files), digestStaged(ctx), nil
}

// Workspace is a staged, digest-verified directory on disk.
type Workspace struct {
	Path           string
	SnapshotDigest string
	SourceDigest   string
	Files          int
	Bytes          int
}

// Remove deletes the staged directory.
func (w *Workspace) Remove() error { return os.RemoveAll(w.Path) }

// StageWorkspace writes spec into parent/name and returns it verified. It
// stages into a private temporary sibling, re-reads and verifies it, and only
// then renames it into place, so a failure leaves nothing behind and a
// half-written workspace never appears under its final name.
func StageWorkspace(parent, name string, spec WorkspaceSpec) (ws *Workspace, err error) {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return nil, invalid("workspace name %q must be a plain, non-hidden name", name)
	}
	files, err := spec.staged()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace parent: %w", err)
	}
	final := filepath.Join(parent, name)
	if _, err := os.Lstat(final); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrWorkspaceExists, final)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("check workspace: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, ".stage-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()
	total := 0
	for _, f := range files {
		dst := filepath.Join(tmp, filepath.FromSlash(f.Path))
		if err = os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, fmt.Errorf("stage %s: %w", f.Path, err)
		}
		mode := os.FileMode(0o600)
		if f.Executable {
			mode = 0o700
		}
		out, oerr := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if oerr != nil {
			return nil, fmt.Errorf("stage %s: %w", f.Path, oerr)
		}
		_, werr := out.Write(f.Content)
		cerr := out.Close()
		if werr != nil || cerr != nil {
			return nil, fmt.Errorf("stage %s: %w", f.Path, errors.Join(werr, cerr))
		}
		if err = os.Chmod(dst, mode); err != nil {
			return nil, fmt.Errorf("stage %s: %w", f.Path, err)
		}
		total += len(f.Content)
	}
	snapshot, source, _ := digestOf(files)
	if err = ValidateWorkspace(tmp, snapshot); err != nil {
		return nil, err
	}
	if err = os.Rename(tmp, final); err != nil {
		return nil, fmt.Errorf("publish workspace: %w", err)
	}
	return &Workspace{Path: final, SnapshotDigest: snapshot, SourceDigest: source, Files: len(files), Bytes: total}, nil
}

// readWorkspace reads dir as it is on disk. Symlinks, devices, git metadata and
// anything outside the staged layout are errors, not skipped.
func readWorkspace(dir string) ([]stagedFile, error) {
	var out []stagedFile
	root := filepath.Clean(dir)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if err := ValidateWorkspacePath(rel); err != nil {
			return err
		}
		top := strings.SplitN(rel, "/", 2)[0]
		if top != ArtifactDir && top != ContextDir && rel != AcceptanceFile {
			return fmt.Errorf("%w: unexpected entry %q", ErrUnsafeWorkspace, rel)
		}
		info, ierr := os.Lstat(p)
		if ierr != nil {
			return ierr
		}
		switch {
		case info.IsDir():
			return nil
		case !info.Mode().IsRegular():
			return fmt.Errorf("%w: %q is not a regular file", ErrUnsafeWorkspace, rel)
		case info.Size() > MaxWorkspaceFileBytes:
			return fmt.Errorf("%w: %s is %d bytes", ErrWorkspaceTooLarge, rel, info.Size())
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out = append(out, stagedFile{Path: rel, Executable: info.Mode().Perm()&0o100 != 0, Content: b})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, checkStaged(out)
}

// WorkspaceDigests recomputes both digests from what is on disk now.
func WorkspaceDigests(dir string) (snapshot, source string, err error) {
	files, err := readWorkspace(dir)
	if err != nil {
		return "", "", err
	}
	return digestOf(files)
}

// ValidateWorkspace recomputes the snapshot digest from disk and compares it
// with want. Run it before a reviewer starts and again after it finishes: a
// mismatch means the staged content changed, was added to, or lost a file.
// This detects tampering; it does not prevent it.
func ValidateWorkspace(dir, want string) error {
	got, _, err := WorkspaceDigests(dir)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: have %s, want %s", ErrDigestMismatch, got, want)
	}
	return nil
}

// SealedString is host-held text that must never reach a reviewer.
type SealedString struct {
	Label string
	Text  string
}

// LeakHit is one place a sealed string (or PR link) was found.
type LeakHit struct {
	Where string
	Label string
}

func (h LeakHit) String() string { return h.Label + " in " + h.Where }

var prLinkRE = regexp.MustCompile(`(?i)https?://\S+/(?:pull|pulls|merge_requests)/\d+`)

func normalizeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// ScanWorkspace looks for sealed strings, and for any pull-request link, in
// every staged path and file and in the prompt. Matching is case- and
// whitespace-insensitive and exact otherwise: a reworded or paraphrased finding
// is not detected, and sealed strings under MinSealedLen are ignored. It is a
// guard against accidental inclusion, not proof of blindness.
func ScanWorkspace(spec WorkspaceSpec, prompt string, sealed []SealedString) ([]LeakHit, error) {
	files, err := spec.staged()
	if err != nil {
		return nil, err
	}
	return scanStaged(files, prompt, sealed), nil
}

// ScanWorkspaceDir scans a workspace as it is on disk.
func ScanWorkspaceDir(dir, prompt string, sealed []SealedString) ([]LeakHit, error) {
	files, err := readWorkspace(dir)
	if err != nil {
		return nil, err
	}
	return scanStaged(files, prompt, sealed), nil
}

func scanStaged(files []stagedFile, prompt string, sealed []SealedString) []LeakHit {
	type needle struct{ label, text string }
	var needles []needle
	for _, s := range sealed {
		if n := normalizeText(s.Text); len(n) >= MinSealedLen {
			needles = append(needles, needle{s.Label, n})
		}
	}
	var hits []LeakHit
	check := func(where, text string) {
		norm := normalizeText(text)
		for _, n := range needles {
			if strings.Contains(norm, n.text) {
				hits = append(hits, LeakHit{Where: where, Label: n.label})
			}
		}
		if prLinkRE.MatchString(text) {
			hits = append(hits, LeakHit{Where: where, Label: "pull request link"})
		}
	}
	sortStaged(files)
	check("prompt", prompt)
	for _, f := range files {
		check("path "+f.Path, f.Path)
		check(f.Path, string(f.Content))
	}
	return hits
}

// LeakError reports leak hits as an error wrapping ErrSealedContent.
func LeakError(hits []LeakHit) error {
	if len(hits) == 0 {
		return nil
	}
	parts := make([]string, len(hits))
	for i, h := range hits {
		parts[i] = h.String()
	}
	return fmt.Errorf("%w: %s", ErrSealedContent, strings.Join(parts, "; "))
}

// BuildBlindedPrompt is the reviewer prompt: fixed instructions plus the full
// acceptance criteria. It names no task, project, round, outcome, reviewer or
// link; those stay in host metadata.
func BuildBlindedPrompt(acceptance string) string {
	var b strings.Builder
	b.WriteString("You are reviewing a submitted work product against its acceptance criteria.\n\n")
	b.WriteString("The working directory contains only:\n")
	b.WriteString("- " + ArtifactDir + "/: the submitted files\n")
	b.WriteString("- " + ContextDir + "/: source context that accompanied the submission, if any\n")
	b.WriteString("- " + AcceptanceFile + ": the acceptance criteria, repeated below\n\n")
	b.WriteString("Report every material defect as a finding with a severity, a claim, a summary and evidence. ")
	b.WriteString("Do not report style preferences. If you find no material defect, report no findings.\n\n")
	b.WriteString("## Acceptance criteria\n\n")
	b.WriteString(acceptance)
	b.WriteString("\n")
	return b.String()
}

// PromptDigest is the content digest of a prompt.
func PromptDigest(prompt string) string {
	sum := sha256.Sum256([]byte("odonian-prompt/v1\x00" + prompt))
	return hex.EncodeToString(sum[:])
}
