package evaluation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func goodSpec() WorkspaceSpec {
	return WorkspaceSpec{
		Artifact:   []WorkspaceFile{{Path: "research/b.md", Content: []byte("bee\n")}, {Path: "research/a.md", Content: []byte("ay\n")}, {Path: "tool.sh", Executable: true, Content: []byte("#!/bin/sh\n")}},
		Context:    []WorkspaceFile{{Path: "manifest.json", Content: []byte(`{"children":[]}`)}},
		Acceptance: "Every claim cites a primary source.\n",
	}
}

func TestWorkspaceDigestIsOrderIndependentAndContentSensitive(t *testing.T) {
	a := goodSpec()
	snapA, srcA, err := a.Digests()
	if err != nil {
		t.Fatal(err)
	}
	b := goodSpec()
	b.Artifact[0], b.Artifact[2] = b.Artifact[2], b.Artifact[0]
	snapB, srcB, _ := b.Digests()
	if snapA != snapB || srcA != srcB {
		t.Fatal("digest depends on input order")
	}

	for name, mut := range map[string]func(*WorkspaceSpec){
		"artifact byte":   func(s *WorkspaceSpec) { s.Artifact[0].Content = []byte("bee!\n") },
		"exec bit":        func(s *WorkspaceSpec) { s.Artifact[0].Executable = true },
		"acceptance":      func(s *WorkspaceSpec) { s.Acceptance += "x" },
		"extra file":      func(s *WorkspaceSpec) { s.Artifact = append(s.Artifact, WorkspaceFile{Path: "z", Content: nil}) },
		"renamed file":    func(s *WorkspaceSpec) { s.Artifact[0].Path = "research/c.md" },
		"context changed": func(s *WorkspaceSpec) { s.Context[0].Content = []byte("{}") },
	} {
		c := goodSpec()
		mut(&c)
		if snap, _, _ := c.Digests(); snap == snapA {
			t.Errorf("%s did not change the snapshot digest", name)
		}
	}
	c := goodSpec()
	c.Artifact[0].Content = []byte("different")
	if _, src, _ := c.Digests(); src != srcA {
		t.Error("source digest must cover only the context section")
	}
	c = goodSpec()
	c.Context[0].Content = []byte("{}")
	if _, src, _ := c.Digests(); src == srcA {
		t.Error("source digest ignored a context change")
	}
	// Boundary ambiguity: moving bytes between path and content must change the digest.
	x := WorkspaceSpec{Artifact: []WorkspaceFile{{Path: "ab", Content: []byte("c")}}, Acceptance: "a"}
	y := WorkspaceSpec{Artifact: []WorkspaceFile{{Path: "a", Content: []byte("bc")}}, Acceptance: "a"}
	sx, _, _ := x.Digests()
	sy, _, _ := y.Digests()
	if sx == sy {
		t.Fatal("digest framing is ambiguous")
	}
}

func TestValidateWorkspacePath(t *testing.T) {
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../../x", "a/./b", "a//b", ".git/config", "sub/.GIT/hooks/x", ".git", "a\\b", "a\x00b", "a\nb", "trailing/", ".."} {
		if err := ValidateWorkspacePath(bad); !errors.Is(err, ErrUnsafeWorkspace) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	for _, ok := range []string{"a", "research/a.md", ".github/workflows/x.yml", "gitignore", "a.git/b", "dir/file with space.md"} {
		if err := ValidateWorkspacePath(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
}

func TestSpecRejectsUnsafeOrAmbiguousContent(t *testing.T) {
	for name, mut := range map[string]func(*WorkspaceSpec){
		"traversal":      func(s *WorkspaceSpec) { s.Artifact[0].Path = "../../escape" },
		"git metadata":   func(s *WorkspaceSpec) { s.Context[0].Path = ".git/HEAD" },
		"case collision": func(s *WorkspaceSpec) { s.Artifact = append(s.Artifact, WorkspaceFile{Path: "RESEARCH/A.md"}) },
		"file and dir":   func(s *WorkspaceSpec) { s.Artifact = append(s.Artifact, WorkspaceFile{Path: "research/a.md/x"}) },
		"no artifact":    func(s *WorkspaceSpec) { s.Artifact = nil },
		"no acceptance":  func(s *WorkspaceSpec) { s.Acceptance = " \n" },
		"oversize file":  func(s *WorkspaceSpec) { s.Artifact[0].Content = make([]byte, MaxWorkspaceFileBytes+1) },
	} {
		s := goodSpec()
		mut(&s)
		if _, _, err := s.Digests(); err == nil {
			t.Errorf("%s: spec accepted", name)
		}
	}
}

func TestStageWorkspaceWritesExactlyTheSpecAndVerifies(t *testing.T) {
	parent := t.TempDir()
	ws, err := StageWorkspace(parent, "w1", goodSpec())
	if err != nil {
		t.Fatal(err)
	}
	wantSnap, wantSrc, _ := goodSpec().Digests()
	if ws.SnapshotDigest != wantSnap || ws.SourceDigest != wantSrc || ws.Files != 5 {
		t.Fatalf("workspace = %+v", ws)
	}
	var seen []string
	filepath.WalkDir(ws.Path, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(ws.Path, p)
			seen = append(seen, filepath.ToSlash(rel))
		}
		return nil
	})
	if strings.Join(seen, ",") != "ACCEPTANCE.md,artifact/research/a.md,artifact/research/b.md,artifact/tool.sh,context/manifest.json" {
		t.Fatalf("staged files = %v", seen)
	}
	if _, err := os.Lstat(filepath.Join(ws.Path, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a .git entry exists in the workspace")
	}
	if info, _ := os.Stat(filepath.Join(ws.Path, "artifact", "tool.sh")); info.Mode().Perm()&0o100 == 0 {
		t.Fatal("exec bit lost")
	}
	if err := ValidateWorkspace(ws.Path, ws.SnapshotDigest); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 1 {
		t.Fatalf("staging left extra entries in the parent: %v", entries)
	}
	if _, err := StageWorkspace(parent, "w1", goodSpec()); !errors.Is(err, ErrWorkspaceExists) {
		t.Fatalf("restaging over an existing workspace: %v", err)
	}
	if err := ws.Remove(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Fatalf("remove left %v", entries)
	}
}

func TestStageWorkspaceLeavesNothingBehindOnFailure(t *testing.T) {
	parent := t.TempDir()
	bad := goodSpec()
	bad.Artifact[0].Path = "../escape"
	if _, err := StageWorkspace(parent, "w", bad); !errors.Is(err, ErrUnsafeWorkspace) {
		t.Fatalf("err = %v", err)
	}
	for _, name := range []string{"", "../x", "a/b", ".hidden"} {
		if _, err := StageWorkspace(parent, name, goodSpec()); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: err = %v", name, err)
		}
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Fatalf("failed staging left %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(parent), "escape")); err == nil {
		t.Fatal("path traversal wrote outside the workspace")
	}
}

func TestValidateWorkspaceDetectsEveryKindOfTampering(t *testing.T) {
	stage := func(t *testing.T) *Workspace {
		ws, err := StageWorkspace(t.TempDir(), "w", goodSpec())
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	tests := map[string]func(root string){
		"edit":       func(r string) { os.WriteFile(filepath.Join(r, "artifact/research/a.md"), []byte("edited"), 0o600) },
		"delete":     func(r string) { os.Remove(filepath.Join(r, "artifact/research/a.md")) },
		"add file":   func(r string) { os.WriteFile(filepath.Join(r, "artifact/late-fix.md"), []byte("x"), 0o600) },
		"add dir":    func(r string) { os.MkdirAll(filepath.Join(r, ".git/objects"), 0o700) },
		"add top":    func(r string) { os.WriteFile(filepath.Join(r, "REVIEWS.md"), []byte("x"), 0o600) },
		"chmod +x":   func(r string) { os.Chmod(filepath.Join(r, "artifact/research/a.md"), 0o700) },
		"acceptance": func(r string) { os.WriteFile(filepath.Join(r, AcceptanceFile), []byte("weaker"), 0o600) },
		"symlink":    func(r string) { os.Symlink("/etc/hostname", filepath.Join(r, "artifact/link")) },
	}
	for name, mut := range tests {
		ws := stage(t)
		mut(ws.Path)
		if err := ValidateWorkspace(ws.Path, ws.SnapshotDigest); err == nil {
			t.Errorf("%s went undetected", name)
		}
	}
}

func TestScanWorkspaceFindsSealedContentAndPRLinks(t *testing.T) {
	sealed := []SealedString{
		{Label: "reviewer finding", Text: "Claim 3 misstates the holding of the cited opinion"},
		{Label: "short", Text: "typo"},
	}
	clean := goodSpec()
	prompt := BuildBlindedPrompt(clean.Acceptance)
	if hits, err := ScanWorkspace(clean, prompt, sealed); err != nil || len(hits) != 0 {
		t.Fatalf("clean workspace flagged: %v %v", hits, err)
	}

	cases := map[string]func(*WorkspaceSpec, *string){
		"finding in artifact": func(s *WorkspaceSpec, _ *string) {
			s.Artifact[0].Content = []byte("note: claim 3 misstates\n  the HOLDING of the cited opinion.")
		},
		"finding in context": func(s *WorkspaceSpec, _ *string) {
			s.Context[0].Content = []byte("Claim 3 misstates the holding of the cited opinion")
		},
		"finding in acceptance": func(s *WorkspaceSpec, _ *string) {
			s.Acceptance += "\nClaim 3 misstates the holding of the cited opinion"
		},
		"finding in prompt": func(_ *WorkspaceSpec, p *string) { *p += "Claim 3 misstates the holding of the cited opinion" },
		"finding in a path": func(s *WorkspaceSpec, _ *string) {
			s.Artifact[0].Path = "Claim 3 misstates the holding of the cited opinion.md"
		},
		"pr link in artifact": func(s *WorkspaceSpec, _ *string) {
			s.Artifact[0].Content = []byte("see https://github.com/acme/repo/pull/467#discussion_r1")
		},
		"pr link in prompt": func(_ *WorkspaceSpec, p *string) { *p += "https://gitlab.example.com/g/p/-/merge_requests/9" },
	}
	for name, mut := range cases {
		s, p := goodSpec(), prompt
		mut(&s, &p)
		hits, err := ScanWorkspace(s, p, sealed)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(hits) == 0 {
			t.Errorf("%s: leak not found", name)
			continue
		}
		if err := LeakError(hits); !errors.Is(err, ErrSealedContent) {
			t.Errorf("%s: LeakError = %v", name, err)
		}
	}
	if LeakError(nil) != nil {
		t.Fatal("no hits must be no error")
	}
}

func TestScanWorkspaceDirScansWhatIsOnDisk(t *testing.T) {
	ws, err := StageWorkspace(t.TempDir(), "w", goodSpec())
	if err != nil {
		t.Fatal(err)
	}
	sealed := []SealedString{{Label: "verdict note", Text: "REJECTED: unsupported claim in row four"}}
	if hits, err := ScanWorkspaceDir(ws.Path, "p", sealed); err != nil || len(hits) != 0 {
		t.Fatalf("hits = %v, %v", hits, err)
	}
	os.WriteFile(filepath.Join(ws.Path, "artifact/research/a.md"), []byte("rejected: unsupported claim in row four"), 0o600)
	if hits, _ := ScanWorkspaceDir(ws.Path, "p", sealed); len(hits) != 1 || hits[0].Label != "verdict note" {
		t.Fatalf("hits = %v", hits)
	}
}

func TestBlindedPromptCarriesCriteriaAndNothingElse(t *testing.T) {
	p := BuildBlindedPrompt("Cite a primary source for each claim.")
	if !strings.Contains(p, "Cite a primary source for each claim.") {
		t.Fatal("full acceptance criteria missing from the prompt")
	}
	for _, banned := range []string{"round", "reject", "clean", "verdict", "outcome", "task ", "http"} {
		if strings.Contains(strings.ToLower(p), banned) {
			t.Errorf("prompt template contains %q", banned)
		}
	}
	if PromptDigest(p) == PromptDigest(p+"x") || PromptDigest(p) != PromptDigest(p) {
		t.Fatal("prompt digest not a function of content")
	}
}
