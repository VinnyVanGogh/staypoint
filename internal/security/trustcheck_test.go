package security

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func trustCtx(t *testing.T) (TrustContext, string, string) {
	t.Helper()
	root := t.TempDir()
	wt := filepath.Join(root, "repo", ".worktrees", "task-1")
	scratch := filepath.Join(root, "scratch", "task-1")
	for _, d := range []string{wt, scratch, filepath.Join(root, "repo", "src")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return TrustContext{
		CWD:       wt,
		Allowed:   []string{wt, scratch},
		Protected: map[string]bool{"main": true, "dev-server": true, "release": true},
		Home:      filepath.Join(root, "home"),
		PushTargets: func(dir string) ([]string, error) {
			return []string{"staypoint/task-1"}, nil
		},
	}, wt, root
}

// Adversarial first: every way to reach a protected branch must be held.
func TestAnalyzeForTrust_ProtectedAlwaysHeld(t *testing.T) {
	tc, _, _ := trustCtx(t)
	cases := []string{
		"gh pr merge 12 --squash",
		"gh -R o/r pr merge 12",
		"gh api -X PUT repos/o/r/pulls/12/merge",
		`gh api graphql -f query='mutation { mergePullRequest(input:{}) { clientMutationId } }'`,
		"git push origin main",
		"git push origin HEAD:main",
		"git push origin +feature:refs/heads/main",
		"git push origin feature:dev-server",
		"git push origin release",
		"git push --all origin",
		"git push --mirror origin",
		"git push origin 'feat*:main*'",
		"git push origin :",
		"git -c remote.origin.push=HEAD:main push",
		"git --git-dir=/x/.git push",
		"cd /tmp && git push origin main",
		"bash -c 'git push origin main'",
		"sh -c \"gh pr merge 3\"",
		"echo $(gh pr merge 3)",
		"env FOO=1 git push origin main",
		"timeout 30 gh pr merge 1",
		"python3 -c \"import subprocess; subprocess.run(['git','push','origin','main'])\"",
		"node -e \"require('child_process').execSync('gh pr merge 1')\"",
		"staypoint ship-review approve task-1",
		"curl -X POST http://127.0.0.1:41421/api/tasks/x/ship-review/approve",
		"git push origin $BRANCH",
		"eval 'git push origin main'",
		"python3 < script.py",
		"unparseable 'quote",
		"ssh build-host 'cd repo && git push origin main'",
		"ssh -p 22 build-host gh pr merge 4",
		"git -c alias.ship='!git push origin main' ship",
		"git ship",
		"git submodule foreach 'git push origin main'",
		"git submodule --quiet foreach --recursive 'git push'",
		"git -c core.sshCommand=x push origin feature",
		"git -ccore.hooksPath=/tmp/h commit -m x && rm -rf /x",
		"git --exec-path=/tmp/evil push origin feature",
		"GIT_DIR=/other/.git git push",
		"env GIT_SSH_COMMAND='sh -c x' git push origin feature",
		"git rebase -x 'git push origin main' HEAD~3",
		"git bisect run gh pr merge 1",
		"ssh -o ProxyCommand='gh pr merge 1' host true",
		"ssh -oLocalCommand=x -oPermitLocalCommand=yes host",
		"ssh -F ./cfg host true",
		"ssh host < deploy.sh",
		"ssh host bash -s < deploy.sh",
		"ssh localhost 'git push origin main'",
		"ssh -o= host 'gh pr merge 1'",
		"ssh -vF ./cfg host true",
		"ssh -4oProxyCommand=x host true",
		"ssh -p22 -oLocalCommand=x host",
		"ssh 127.0.0.2 'git push origin main'",
		"ssh '[::1]' 'git push origin main'",
		"ssh localhost. 'git push origin main'",
		"sudo GIT_DIR=/o/.git git push",
		"GIT_DIR=/o/.git sudo git push",
		"GIT_DIR=/o/.git timeout 5 git push",
		"ssh -o HostName=127.0.0.1 build 'git push origin main'",
		"ssh -J jump host true",
		"ssh 127.1 'git push origin main'",
		"ssh 0x7f000001 'gh pr merge 1'",
	}
	for _, c := range cases {
		if f := AnalyzeForTrust(c, tc); !f.Protected {
			t.Errorf("%q: not held as protected (facts %+v)", c, f)
		}
	}
}

func TestAnalyzeForTrust_BarePushUsesPushTargets(t *testing.T) {
	tc, _, _ := trustCtx(t)
	if f := AnalyzeForTrust("git push", tc); f.Protected {
		t.Fatalf("bare push of a task branch held: %+v", f)
	}
	tc.PushTargets = func(string) ([]string, error) { return []string{"staypoint/task-1", "main"}, nil }
	if f := AnalyzeForTrust("git push", tc); !f.Protected {
		t.Fatal("bare push whose upstream is main not held")
	}
	tc.PushTargets = func(string) ([]string, error) { return nil, errors.New("detached") }
	if f := AnalyzeForTrust("git push -u origin HEAD", tc); !f.Protected {
		t.Fatal("push HEAD with unknown branch not held (fail closed)")
	}
	tc.PushTargets = nil
	if f := AnalyzeForTrust("git push", tc); !f.Protected {
		t.Fatal("bare push without a resolver not held (fail closed)")
	}
}

func TestAnalyzeForTrust_NonProtectedPushAndSSHPass(t *testing.T) {
	tc, _, _ := trustCtx(t)
	for _, c := range []string{
		"git push origin staypoint/task-1",
		"git push -u origin HEAD:staypoint/task-1",
		"git push --force-with-lease origin staypoint/task-1",
		"ssh build-host 'make test'",
		"scp out.txt build-host:/tmp/",
		"gh pr create --title x --body y",
		"gh pr view 12",
	} {
		if f := AnalyzeForTrust(c, tc); f.Protected || f.DeleteOutside {
			t.Errorf("%q: held under trust: %+v", c, f)
		}
	}
}

func TestAnalyzeForTrust_DeleteOutsideWorktree(t *testing.T) {
	tc, wt, root := trustCtx(t)
	link := filepath.Join(wt, "escape")
	if err := os.Symlink(filepath.Join(root, "repo", "src"), link); err != nil {
		t.Fatal(err)
	}
	outside := []string{
		"rm -rf /",
		"rm -rf ../../src",
		"rm -rf " + filepath.Join(root, "repo", "src"),
		"rm -rf " + wt, // the worktree itself is not inside it
		"rm -rf ~/Documents",
		"rm -rf $HOME/x",
		"rm -rf \"$DIR\"",
		"rmdir /etc/x",
		"unlink /etc/hosts",
		"truncate -s 0 /etc/hosts",
		"echo > /etc/hosts",
		"cat x > ../../README.md",
		"find / -name '*.log' -delete",
		"find . -delete && cd / && find . -delete",
		"find /var -exec rm {} ';'",
		"rsync -a --delete src/ /backup/",
		"rsync -a --delete src/ host:/backup/",
		"git -C " + filepath.Join(root, "repo") + " clean -fdx",
		"git worktree remove " + wt,
		"cd / && rm -rf tmpdir",
		"cd $X && rm -rf a",
		"pushd /etc && rm -f hosts",
		"find . -name x | xargs rm -rf",
		"rm -rf escape/inner",
		"bash -c 'rm -rf /opt/x'",
		"python3 -c 'import shutil; shutil.rmtree(\"/opt\")'",
		"node -e \"require('fs').rmSync('/opt',{recursive:true})\"",
		"sudo rm -rf /opt",
		"rm -rf /tmp/*",
		"./cleanup.sh",
		"echo x | tee /etc/hosts",
		"sudo dd if=/dev/zero of=/etc/hosts",
		"mv ~/notes.txt ./",
		"cp out.txt /etc/hosts",
		"ln -sf x /usr/local/bin/tool",
		"mv -t /etc a.txt",
		"mv --target-directory=/etc a.txt",
		"cp -t/etc out.txt",
		"cp -ft/etc out.txt",
		"install -m 755 tool /usr/local/bin/",
		"ssh me@127.0.0.1 'rm -rf ~/Documents'",
		"cp -pt /etc out.txt",
		"cp -S .bak x.txt /etc/hosts",
		"install -m755 tool /usr/local/bin/tool",
		"ln -s target /usr/local/bin/x",
		"cp --target /etc a.txt b.txt",
		"cp --target-dir=/etc a.txt",
		"cp --t=/etc a.txt",
		"cp a.txt -t /etc b.txt",
	}
	for _, c := range outside {
		if f := AnalyzeForTrust(c, tc); !f.DeleteOutside {
			t.Errorf("%q: delete outside the worktree not detected (facts %+v)", c, f)
		}
	}
}

func TestAnalyzeForTrust_DeleteInsideAllowed(t *testing.T) {
	tc, wt, _ := trustCtx(t)
	inside := []string{
		"rm -rf build",
		"rm -rf ./node_modules dist/*.js",
		"rm -f " + filepath.Join(wt, "x.txt"),
		"rm -rf " + filepath.Join(tc.Allowed[1], "tmp"),
		"find . -name '*.orig' -delete",
		"git clean -fdx",
		"echo hi > out.txt",
		"go test ./... 2>&1 > /dev/null",
		"cat a >> log.txt",
		"echo x | tee -a /etc/hosts",
		"mv a.txt b.txt",
		"cp " + "/etc/hosts" + " ./hosts.copy",
		"ssh build-host 'rm -rf /tmp/build && make'",
	}
	for _, c := range inside {
		if f := AnalyzeForTrust(c, tc); f.DeleteOutside || f.Protected {
			t.Errorf("%q: held, want auto-approvable: %+v", c, f)
		}
	}
}

func TestAnalyzeForTrust_ScriptsByContent(t *testing.T) {
	tc, wt, _ := trustCtx(t)
	p := filepath.Join(wt, "run.sh")
	tc.Scripts = []ScriptHash{{Path: p, Trusted: true, Content: "#!/bin/sh\ngo test ./...\nrm -rf build\n"}}
	if f := AnalyzeForTrust("bash run.sh", tc); f.DeleteOutside || f.Protected {
		t.Fatalf("harmless captured script held: %+v", f)
	}
	tc.Scripts[0].Trusted = false
	if f := AnalyzeForTrust("bash run.sh", tc); !f.DeleteOutside || !f.Protected {
		t.Fatalf("unpinned script (may change before it runs) not treated as unknown: %+v", f)
	}
	tc.Scripts = []ScriptHash{{Path: p, Trusted: true, Content: "#!/bin/sh\ngh pr merge 1\n"}}
	if f := AnalyzeForTrust("bash run.sh", tc); !f.Protected {
		t.Fatal("script that merges not held")
	}
	tc.Scripts = []ScriptHash{{Path: p, Trusted: true, Content: "#!/bin/sh\nrm -rf /\n" + TruncatedMarker}}
	if f := AnalyzeForTrust("bash run.sh", tc); !f.DeleteOutside || !f.Protected {
		t.Fatalf("truncated script not treated as unknown: %+v", f)
	}
}
