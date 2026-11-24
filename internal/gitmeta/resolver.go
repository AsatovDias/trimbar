package gitmeta

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AsatovDias/trimbar/internal/anthropic"
)

const defaultCacheTTL = 3 * time.Second

// cwdPatterns match the working directory lines injected by Claude Code into its system prompt.
var cwdPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^\s*(?:primary\s+working\s+directory|current\s+working\s+directory|working\s+directory|cwd)\s*:\s*(.+?)\s*$`),
	regexp.MustCompile(`(?i)<cwd>\s*([^<\r\n]+?)\s*</cwd>`),
}

// claudeCodeSessionRegex matches Claude Code's metadata.user_id format:
// e.g. "user_<hash>_account_<uuid>_session_<uuid>"
var claudeCodeSessionRegex = regexp.MustCompile(`(?i)_session_([a-z0-9-]+)`)

// GitMeta describes the local Git repository and branch resolved for a directory.
type GitMeta struct {
	CWD      string
	RepoRoot string
	Repo     string
	Branch   string
	IsGit    bool
}

// Attribution combines Git metadata and a stable session identifier for a request.
type Attribution struct {
	CWD       string
	Repo      string
	Branch    string
	SessionID string
	IsGit     bool
}

type cacheEntry struct {
	meta      GitMeta
	expiresAt time.Time
}

// Resolver extracts cwd from Claude Code system prompts and resolves local Git
// repository, branch, and stable session_id with lightweight caching.
type Resolver struct {
	ttl   time.Duration
	mu    sync.RWMutex
	cache map[string]cacheEntry
}

// NewResolver creates a Resolver with the default short cache TTL.
func NewResolver() *Resolver {
	return &Resolver{
		ttl:   defaultCacheTTL,
		cache: make(map[string]cacheEntry),
	}
}

// Resolve inspects an anthropic.InspectionResult and returns full Attribution
// (cwd, git repo, git branch, and session_id).
// If cwd is absent from the system prompt or is outside a Git repo, it falls back
// to file/directory paths seen in the first tool_use calls (Read/Glob/Grep/Edit).
func (r *Resolver) Resolve(insp *anthropic.InspectionResult) Attribution {
	if insp == nil {
		return Attribution{SessionID: "sess_unknown"}
	}
	cwd := ExtractCWD(insp.SystemPromptText)
	gm := r.ResolveGitMeta(cwd)

	// Fallback: resolve Git repository from tool_use file paths when system prompt lacks cwd.
	if !gm.IsGit && len(insp.ToolFilePaths) > 0 {
		for _, candidate := range insp.ToolFilePaths {
			if candidate == "" {
				continue
			}
			fb := r.ResolveGitMeta(candidate)
			if fb.IsGit {
				gm = fb
				if cwd == "" {
					cwd = fb.RepoRoot
				}
				break
			}
		}
	}

	sessionID := DeriveSessionID(insp, cwd)

	return Attribution{
		CWD:       cwd,
		Repo:      gm.Repo,
		Branch:    gm.Branch,
		SessionID: sessionID,
		IsGit:     gm.IsGit,
	}
}

// ExtractCWD extracts the working directory path from a Claude Code system prompt.
func ExtractCWD(systemPrompt string) string {
	if strings.TrimSpace(systemPrompt) == "" {
		return ""
	}
	for _, re := range cwdPatterns {
		matches := re.FindStringSubmatch(systemPrompt)
		if len(matches) < 2 {
			continue
		}
		candidate := strings.TrimSpace(matches[1])
		candidate = strings.Trim(candidate, "`\"'")
		candidate = strings.TrimRight(candidate, ".,;")
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// ResolveGitMeta resolves the local Git repository name and current branch for cwd.
// It walks parent directories purely via the filesystem (no git subprocess overhead).
func (r *Resolver) ResolveGitMeta(cwd string) GitMeta {
	cleaned := strings.TrimSpace(cwd)
	if cleaned == "" {
		return GitMeta{}
	}
	cleaned = filepath.Clean(cleaned)

	now := time.Now()
	if r != nil {
		r.mu.RLock()
		if entry, ok := r.cache[cleaned]; ok && now.Before(entry.expiresAt) {
			r.mu.RUnlock()
			return entry.meta
		}
		r.mu.RUnlock()
	}

	meta := resolveGitMetaFromDisk(cleaned)

	if r != nil && r.ttl > 0 {
		r.mu.Lock()
		r.cache[cleaned] = cacheEntry{
			meta:      meta,
			expiresAt: now.Add(r.ttl),
		}
		r.mu.Unlock()
	}
	return meta
}

// DeriveSessionID determines a stable session_id for a Claude Code conversation.
// Priority:
//  1. Explicit metadata.session_id
//  2. Claude Code metadata.user_id embedded "_session_<uuid>"
//  3. Deterministic SHA-256 fingerprint of (cwd, first user message, system prompt prefix)
func DeriveSessionID(insp *anthropic.InspectionResult, cwd string) string {
	if insp == nil {
		return "sess_unknown"
	}
	if s := strings.TrimSpace(insp.MetadataSessionID); s != "" {
		return s
	}
	if uid := strings.TrimSpace(insp.MetadataUserID); uid != "" {
		if m := claudeCodeSessionRegex.FindStringSubmatch(uid); len(m) == 2 && m[1] != "" {
			return m[1]
		}
	}

	h := sha256.New()
	normCWD := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(cwd), "\\", "/"))
	h.Write([]byte("cwd:"))
	h.Write([]byte(normCWD))
	h.Write([]byte("\nuser:"))
	h.Write([]byte(strings.TrimSpace(insp.FirstUserText)))
	h.Write([]byte("\nsys:"))
	sysPrefix := insp.SystemPromptText
	if len(sysPrefix) > 512 {
		sysPrefix = sysPrefix[:512]
	}
	h.Write([]byte(sysPrefix))

	sum := hex.EncodeToString(h.Sum(nil))
	return "sess_" + sum[:12]
}

func resolveGitMetaFromDisk(cwd string) GitMeta {
	meta := GitMeta{CWD: cwd}

	repoRoot, gitDir, commonDir := findGitDirs(cwd)
	if gitDir == "" {
		return meta
	}

	meta.IsGit = true
	meta.RepoRoot = repoRoot
	meta.Branch = readGitBranch(gitDir)

	// Prefer remote origin "owner/repo", falling back to repository folder basename.
	configDir := commonDir
	if configDir == "" {
		configDir = gitDir
	}
	if remoteRepo := readOriginRepoName(filepath.Join(configDir, "config")); remoteRepo != "" {
		meta.Repo = remoteRepo
	} else {
		meta.Repo = filepath.Base(repoRoot)
	}
	return meta
}

// findGitDirs walks up from startDir to locate the Git repository root, active gitDir,
// and commonDir (for linked worktrees).
func findGitDirs(startDir string) (repoRoot, gitDir, commonDir string) {
	dir := startDir
	// If startDir points to a file or doesn't exist yet, still try walking parents.
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		dir = filepath.Dir(dir)
	}

	for {
		dotGit := filepath.Join(dir, ".git")
		info, err := os.Stat(dotGit)
		if err == nil {
			if info.IsDir() {
				return dir, dotGit, dotGit
			}
			// Worktree or submodule: .git is a file containing "gitdir: <path>"
			if gd := parseGitDirFile(dotGit, dir); gd != "" {
				cd := resolveCommonDir(gd)
				return dir, gd, cd
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", ""
		}
		dir = parent
	}
}

func parseGitDirFile(dotGitFile, baseDir string) string {
	data, err := os.ReadFile(dotGitFile)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(strings.ToLower(line), prefix) {
		return ""
	}
	target := strings.TrimSpace(line[len(prefix):])
	if target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(baseDir, target)
	}
	return filepath.Clean(target)
}

func resolveCommonDir(gitDir string) string {
	commondirPath := filepath.Join(gitDir, "commondir")
	data, err := os.ReadFile(commondirPath)
	if err != nil {
		return gitDir
	}
	rel := strings.TrimSpace(string(data))
	if rel == "" {
		return gitDir
	}
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel)
	}
	return filepath.Clean(filepath.Join(gitDir, rel))
}

func readGitBranch(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	const refPrefix = "ref:"
	if strings.HasPrefix(head, refPrefix) {
		ref := strings.TrimSpace(head[len(refPrefix):])
		if strings.HasPrefix(ref, "refs/heads/") {
			return strings.TrimPrefix(ref, "refs/heads/")
		}
		return ref
	}
	// Detached HEAD: return short commit hash.
	if len(head) >= 7 {
		return "detached@" + head[:7]
	}
	return head
}

func readOriginRepoName(configPath string) string {
	f, err := os.Open(configPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	inOrigin := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			inOrigin = section == `remote "origin"`
			continue
		}
		if !inOrigin {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "url") {
			return parseRemoteRepoName(strings.TrimSpace(val))
		}
	}
	return ""
}

// parseRemoteRepoName extracts "owner/repo" (or "repo") from HTTPS or SSH git remote URLs.
// Examples:
//   - https://github.com/trimbar/trimbar.git -> trimbar/trimbar
//   - git@github.com:trimbar/trimbar.git     -> trimbar/trimbar
func parseRemoteRepoName(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return ""
	}
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")

	// Handle SCP-style SSH URLs: git@github.com:owner/repo
	if !strings.Contains(u, "://") {
		if idx := strings.Index(u, ":"); idx >= 0 {
			u = u[idx+1:]
		}
	} else {
		if idx := strings.Index(u, "://"); idx >= 0 {
			rest := u[idx+3:]
			if slash := strings.Index(rest, "/"); slash >= 0 {
				u = rest[slash+1:]
			} else {
				return ""
			}
		}
	}

	u = strings.Trim(u, "/")
	parts := strings.Split(u, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return u
}
