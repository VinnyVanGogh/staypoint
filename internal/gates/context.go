package gates

import (
	"context"
	"database/sql"
	"os"
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
	// ReadFile reads scripts to hash (default os.ReadFile).
	ReadFile func(string) ([]byte, error)
	// RepoRoot returns the git toplevel for a dir, "" if none (default: git).
	RepoRoot func(dir string) string
}

func (r *Resolver) readFile() func(string) ([]byte, error) {
	if r.ReadFile != nil {
		return r.ReadFile
	}
	return os.ReadFile
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

// Resolve completes in: a known task supplies repo and org; otherwise the
// repo is the git toplevel of cwd. It returns the scripts the command runs,
// with contents for advisors.
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
	refs := security.ScriptRefs(in.Cmdline, in.CWD, r.readFile(), ScriptContentLimit)
	in.Scripts = make([]security.ScriptHash, 0, len(refs))
	for _, s := range refs {
		in.Scripts = append(in.Scripts, security.ScriptHash{Path: s.Path, SHA256: s.SHA256, Trusted: s.Trusted})
	}
	return refs
}

// CurrentScripts re-reads the scripts of a stored request (for pinning at
// approval time and for reviewers).
func (r *Resolver) CurrentScripts(gr *security.GateRequest) []security.ScriptRef {
	return security.ScriptRefs(gr.Cmdline, gr.CWD, r.readFile(), ScriptContentLimit)
}
