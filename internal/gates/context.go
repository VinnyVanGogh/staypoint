package gates

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// ScriptContentLimit bounds script text sent to advisors.
const ScriptContentLimit = 4096

// Resolver fills in a request's task, repo and org. Its seams keep tests off
// the real filesystem and git.
type Resolver struct {
	DB *sql.DB
	// RepoRoot returns the git toplevel for a dir, "" if none (default: git).
	RepoRoot func(dir string) string
}

func gitRepoRoot(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := gitexec.Command(ctx, "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return filepath.Clean(strings.TrimSpace(string(out)))
}

// HookScript is a script snapshot the pre-tool hook sends with a request:
// the exact bytes it will run when the command is pinned.
type HookScript struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ScriptsFromHook hashes the hook's snapshots. They are trusted (usable by
// allow rules) only when the hook pinned the command to those bytes.
func ScriptsFromHook(list []HookScript, pinned bool) []security.ScriptHash {
	out := make([]security.ScriptHash, 0, len(list))
	for _, h := range list {
		sum := sha256.Sum256([]byte(h.Content))
		out = append(out, security.ScriptHash{Path: h.Path, SHA256: hex.EncodeToString(sum[:]), Trusted: pinned,
			Content: truncate(h.Content, ScriptContentLimit)})
	}
	return out
}

// AdviceScripts turns stored script snapshots into advisor input.
func AdviceScripts(list []security.ScriptHash) []security.ScriptRef {
	out := make([]security.ScriptRef, 0, len(list))
	for _, s := range list {
		out = append(out, security.ScriptRef{Path: s.Path, SHA256: s.SHA256, Trusted: s.Trusted, Content: s.Content})
	}
	return out
}

// Resolve completes in: a known task supplies repo and org; otherwise the
// repo is the git toplevel of cwd. When the hook sent no script snapshots
// (in.Scripts == nil) the daemon reads them itself; those are never trusted,
// since nothing pins what will run. It returns advisor input.
func (r *Resolver) Resolve(in *security.GateRequestInput) []security.ScriptRef {
	if in.TaskID != "" && r.DB != nil {
		var repo, org string
		err := r.DB.QueryRow(`SELECT COALESCE(repo_path,''), COALESCE(organization,'') FROM tasks WHERE id = ?`, in.TaskID).Scan(&repo, &org)
		if err == nil {
			in.Repo, in.Org = filepath.Clean(repo), org
			if repo == "" {
				in.Repo = ""
			}
		}
	}
	if in.Repo == "" && in.CWD != "" && filepath.IsAbs(in.CWD) {
		root := r.RepoRoot
		if root == nil {
			root = gitRepoRoot
		}
		in.Repo = root(in.CWD)
	}
	if in.Scripts != nil {
		return AdviceScripts(in.Scripts)
	}
	refs := security.ScriptRefs(in.Cmdline, in.CWD, security.NewSnapshotter(), ScriptContentLimit)
	in.Scripts = make([]security.ScriptHash, 0, len(refs))
	for _, s := range refs {
		in.Scripts = append(in.Scripts, security.ScriptHash{Path: s.Path, SHA256: s.SHA256, Content: s.Content})
	}
	return refs
}
