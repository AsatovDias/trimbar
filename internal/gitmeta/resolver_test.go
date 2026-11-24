package gitmeta

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trimbar/trimbar/internal/anthropic"
)

func TestExtractCWD(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   string
	}{
		{
			name: "Claude Code Unix env block",
			prompt: `You are Claude Code, Anthropic's official CLI for Claude.
Here is useful information about the environment you are running in:
<env>
Working directory: /Users/alice/dev/trimbar
Is directory a git repo: Yes
Platform: darwin
</env>`,
			want: "/Users/alice/dev/trimbar",
		},
		{
			name: "Primary working directory Windows path",
			prompt: "Environment:\nPrimary working directory: C:\\Users\\Aser\\Desktop\\Trimbar\nOS: windows",
			want:   `C:\Users\Aser\Desktop\Trimbar`,
		},
		{
			name:   "Backticked cwd line",
			prompt: "cwd: `/home/dev/my-app`",
			want:   "/home/dev/my-app",
		},
		{
			name:   "Missing cwd",
			prompt: "You are a helpful assistant.",
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractCWD(tc.prompt); got != tc.want {
				t.Errorf("ExtractCWD() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveGitMetaFromFilesystem(t *testing.T) {
	// 1. Standard Git repo with nested subdirectory and SSH origin remote
	repoRoot := t.TempDir()
	dotGit := filepath.Join(repoRoot, ".git")
	nested := filepath.Join(repoRoot, "internal", "proxy")
	if err := os.MkdirAll(dotGit, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dotGit, "HEAD"), []byte("ref: refs/heads/feat/cache-inspector\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	gitConfig := `[core]
	repositoryformatversion = 0
[remote "origin"]
	url = git@github.com:trimbar/trimbar.git
	fetch = +refs/heads/*:refs/remotes/origin/*
`
	if err := os.WriteFile(filepath.Join(dotGit, "config"), []byte(gitConfig), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	resolver := NewResolver()
	meta := resolver.ResolveGitMeta(nested)
	if !meta.IsGit {
		t.Fatalf("expected IsGit=true for nested dir")
	}
	if meta.Repo != "trimbar/trimbar" {
		t.Errorf("Repo = %q, want trimbar/trimbar", meta.Repo)
	}
	if meta.Branch != "feat/cache-inspector" {
		t.Errorf("Branch = %q, want feat/cache-inspector", meta.Branch)
	}

	// 2. Linked Git worktree (.git is a file pointing to gitdir + commondir)
	worktreeRoot := t.TempDir()
	wtGitDir := filepath.Join(dotGit, "worktrees", "wt1")
	if err := os.MkdirAll(wtGitDir, 0o755); err != nil {
		t.Fatalf("mkdir wtGitDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtGitDir, "HEAD"), []byte("ref: refs/heads/fix/sse-flush\n"), 0o644); err != nil {
		t.Fatalf("write wt HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtGitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatalf("write wt commondir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeRoot, ".git"), []byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatalf("write worktree .git file: %v", err)
	}

	wtMeta := resolver.ResolveGitMeta(worktreeRoot)
	if !wtMeta.IsGit {
		t.Fatalf("expected IsGit=true for worktree")
	}
	if wtMeta.Repo != "trimbar/trimbar" {
		t.Errorf("worktree Repo = %q, want trimbar/trimbar", wtMeta.Repo)
	}
	if wtMeta.Branch != "fix/sse-flush" {
		t.Errorf("worktree Branch = %q, want fix/sse-flush", wtMeta.Branch)
	}

	// 3. Detached HEAD
	detachedRoot := t.TempDir()
	detachedGit := filepath.Join(detachedRoot, ".git")
	if err := os.MkdirAll(detachedGit, 0o755); err != nil {
		t.Fatalf("mkdir detached .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(detachedGit, "HEAD"), []byte("a1b2c3d4e5f60708090a0b0c0d0e0f1011121314\n"), 0o644); err != nil {
		t.Fatalf("write detached HEAD: %v", err)
	}
	dMeta := resolver.ResolveGitMeta(detachedRoot)
	if dMeta.Branch != "detached@a1b2c3d" {
		t.Errorf("detached Branch = %q, want detached@a1b2c3d", dMeta.Branch)
	}
	if dMeta.Repo != filepath.Base(detachedRoot) {
		t.Errorf("detached fallback Repo = %q, want %q", dMeta.Repo, filepath.Base(detachedRoot))
	}

	// 4. Non-git directory
	nonGit := t.TempDir()
	ngMeta := resolver.ResolveGitMeta(nonGit)
	if ngMeta.IsGit {
		t.Errorf("expected IsGit=false for non-git dir, got %+v", ngMeta)
	}
}

func TestDeriveSessionIDAndFullResolve(t *testing.T) {
	repoRoot := t.TempDir()
	dotGit := filepath.Join(repoRoot, ".git")
	if err := os.MkdirAll(dotGit, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dotGit, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dotGit, "config"), []byte("[remote \"origin\"]\n\turl = https://github.com/acme/webapp.git\n"), 0o644)

	resolver := NewResolver()

	// Case 1: Claude Code metadata.user_id with embedded _session_<uuid>
	inspWithMeta := &anthropic.InspectionResult{
		SystemPromptText: "Working directory: " + repoRoot,
		MetadataUserID:   "user_abc123_account_1111_session_9f8e7d6c-5b4a-3210",
		FirstUserText:    "Fix login bug",
	}
	attr := resolver.Resolve(inspWithMeta)
	if attr.Repo != "acme/webapp" || attr.Branch != "main" {
		t.Errorf("attr repo/branch = (%q, %q), want (acme/webapp, main)", attr.Repo, attr.Branch)
	}
	if attr.SessionID != "9f8e7d6c-5b4a-3210" {
		t.Errorf("SessionID = %q, want 9f8e7d6c-5b4a-3210", attr.SessionID)
	}

	// Case 2: Fallback deterministic session_id across turns of the same conversation
	turn1 := &anthropic.InspectionResult{
		SystemPromptText: "Working directory: " + repoRoot + "\nYou are Claude Code.",
		FirstUserText:    "Refactor storage package",
	}
	turn2 := &anthropic.InspectionResult{
		SystemPromptText: "Working directory: " + repoRoot + "\nYou are Claude Code.",
		FirstUserText:    "Refactor storage package",
	}
	otherConv := &anthropic.InspectionResult{
		SystemPromptText: "Working directory: " + repoRoot + "\nYou are Claude Code.",
		FirstUserText:    "Add dark mode toggle",
	}

	s1 := resolver.Resolve(turn1).SessionID
	s2 := resolver.Resolve(turn2).SessionID
	s3 := resolver.Resolve(otherConv).SessionID

	if s1 == "" || s1 != s2 {
		t.Errorf("expected identical fallback session IDs for same conversation: %q vs %q", s1, s2)
	}
	if s1 == s3 {
		t.Errorf("expected different session ID for a different conversation, both got %q", s1)
	}
}

func TestResolveGitMetaFallbackFromToolFilePaths(t *testing.T) {
	repoRoot := t.TempDir()
	dotGit := filepath.Join(repoRoot, ".git")
	srcDir := filepath.Join(repoRoot, "internal", "trimmer")
	if err := os.MkdirAll(dotGit, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir srcDir: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dotGit, "HEAD"), []byte("ref: refs/heads/feat/audit\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dotGit, "config"), []byte("[remote \"origin\"]\n\turl = https://github.com/AsatovDias/trimbar.git\n"), 0o644)

	targetFile := filepath.Join(srcDir, "trimmer.go")
	_ = os.WriteFile(targetFile, []byte("package trimmer\n"), 0o644)

	// System prompt contains NO Working directory line at all!
	insp := &anthropic.InspectionResult{
		SystemPromptText: "You are an automated coding assistant.",
		ToolFilePaths:    []string{targetFile},
		FirstUserText:    "Check trimmer.go",
	}

	resolver := NewResolver()
	attr := resolver.Resolve(insp)
	if !attr.IsGit {
		t.Fatalf("expected IsGit=true via ToolFilePaths fallback, got %+v", attr)
	}
	if attr.Repo != "AsatovDias/trimbar" || attr.Branch != "feat/audit" {
		t.Errorf("fallback attribution = (%q, %q), want (AsatovDias/trimbar, feat/audit)", attr.Repo, attr.Branch)
	}
	if attr.CWD != repoRoot {
		t.Errorf("fallback CWD = %q, want %q", attr.CWD, repoRoot)
	}
}
