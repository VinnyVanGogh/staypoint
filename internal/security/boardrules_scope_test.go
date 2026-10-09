package security

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// task-6e2bcd75: self-protection holds what changes StayPoint (running the
// reinstall script, the installed binaries, the live state, the guards, the
// gate's env), not commands that only name it. task-800b532d looped for six
// hours on `bash -n`, `git add` and `sed -n` of the reinstall script.
func TestBoardRulesSelfProtectionScope(t *testing.T) {
	const task = "task-T1"
	held := []string{
		// Running the reinstall script, however it is wrapped.
		"bash scripts/reinstall-daemon.sh",
		"./scripts/reinstall-daemon.sh",
		"scripts/reinstall-daemon.sh --allow-dev-build",
		"sh -c 'scripts/reinstall-daemon.sh'",
		`zsh -lc "cd /r && ./scripts/reinstall-daemon.sh"`,
		"bash -c '\"$0\"' scripts/reinstall-daemon.sh",
		"env FOO=1 scripts/reinstall-daemon.sh",
		"nohup ./scripts/reinstall-daemon.sh &",
		"timeout 600 bash scripts/reinstall-daemon.sh",
		"bash -x scripts/reinstall-daemon.sh",
		"bash -n x.sh && bash scripts/reinstall-daemon.sh",
		"bash -n scripts/reinstall-daemon.sh; bash scripts/reinstall-daemon.sh",
		"(cd scripts && ./reinstall-daemon.sh)",
		"cd scripts && bash reinstall-daemon.sh",
		"echo $(scripts/reinstall-daemon.sh)",
		"source scripts/reinstall-daemon.sh",
		". scripts/reinstall-daemon.sh",
		"bash < scripts/reinstall-daemon.sh",
		"cat scripts/reinstall-daemon.sh | bash",
		"cat scripts/reinstall-daemon.sh | grep -v '^#' | sh",
		"echo scripts/reinstall-daemon.sh | xargs bash",
		"bash <<'EOF'\nscripts/reinstall-daemon.sh\nEOF",
		"F=scripts/reinstall-daemon.sh; bash $F",
		"for f in scripts/reinstall-daemon.sh; do bash $f; done",
		"git -c alias.r='!scripts/reinstall-daemon.sh' r",
		"git rebase -x scripts/reinstall-daemon.sh main",
		"go test -exec scripts/reinstall-daemon.sh ./...",
		// A copy or symlink of the script, run now or later.
		"ln -s scripts/reinstall-daemon.sh /tmp/r && /tmp/r",
		"cp scripts/reinstall-daemon.sh /tmp/r.sh; bash /tmp/r.sh",
		"cat scripts/reinstall-daemon.sh > /tmp/r.sh",
		"sed -n p scripts/reinstall-daemon.sh > /tmp/r.sh",
		"sed -n 'w /tmp/r.sh' scripts/reinstall-daemon.sh",
		"sed -n '1e scripts/reinstall-daemon.sh' x",
		"sed 's/x/scripts\\/reinstall-daemon.sh/e' x",
		"sed -f cmds.sed scripts/reinstall-daemon.sh",
		"sed -nf cmds.sed scripts/reinstall-daemon.sh",
		"sed -n 's_x_launchctl list_e' f",
		"sed -n -e p -e '1e scripts/reinstall-daemon.sh' x",
		"sed -ne '1w /tmp/r.sh' scripts/reinstall-daemon.sh",
		"sed --file=cmds.sed scripts/reinstall-daemon.sh",
		"sed -n '1e launchctl kickstart -k gui/501/com.staypoint.daemon' x",
		"find . -name x -exec launchctl load {} \\;",
		`python3 -c "import os; os.system('launchctl kickstart -k gui/501/x')"`,
		"grep -l x launchctl.txt | xargs launchctl load",
		"tee /tmp/r.sh < scripts/reinstall-daemon.sh",
		// Commands read from input or a file cannot be checked.
		"xargs bash < list",
		"xargs -n1 sh < list",
		"bash < cmds.txt",
		"cat list | sh -s",
		// Installed binaries, launchd, the daemon process.
		"launchctl bootout gui/501/com.staypoint.daemon",
		"cp bin/staypointd ~/.local/bin/staypointd",
		"go build -o ~/.local/bin/staypointd ./cmd/staypointd",
		"go install ./cmd/staypoint",
		"pkill -f staypointd",
		"killall staypointd",
		"kill $(pgrep staypointd)",
		// Live state and guard config.
		"cat ~/.gemini/config/hooks.json",
		"cat ~/.staypoint/config.toml",
		"cat ~/.staypoint/handoffs/task-OTHER/latest.md",
		"cat ~/.staypoint/handoffs/task-T10/latest.md",
		"cat ~/.staypoint/handoffs/task-T1/../../board_token",
		"cat ~/.staypoint/handoffs/task-T1/latest.md ~/.staypoint/board_token",
		"cat ~/.staypoint/handoffs/task-T1/latest.md; cat ~/.staypoint/auth_token",
		"cat ~/.staypoint/handoffs/task-T1/latest.md > /tmp/x",
		"cp ~/.staypoint/handoffs/task-T1/latest.md ~/.staypoint/config.toml",
		"sed -i '' s/a/b/ ~/.staypoint/handoffs/task-T1/latest.md",
		"rm ~/.staypoint/handoffs/task-T1/latest.md",
		"cat ~/.staypoint/handoffs/task-T1/{..,x}/{..,y}/board_token",
		"cat ~/.staypoint/handoffs/task-T1/.?/.?/board_token",
		"cat ~/.staypoint/handoffs/task-T1/.*/.*/auth_token",
		"cat ~/.staypoint/handoffs/task-T1/[.][.]/[.][.]/auth_token",
		"cat ~/.staypoint/handoffs/task-T1/$(echo ../..)/auth_token",
		"HOME=/tmp/h cat ~/.staypoint/handoffs/task-T1/latest.md",
		// Syntax-check flags that do not stop the run.
		"bash -c 'scripts/reinstall-daemon.sh' -n",
		"bash -i -n scripts/reinstall-daemon.sh",
		"bash -l -n scripts/reinstall-daemon.sh",
		"bash --login -n scripts/reinstall-daemon.sh",
		"bash -s -n < scripts/reinstall-daemon.sh",
		// Named in an assignment or a wrapper's args.
		"BASH_ENV=scripts/reinstall-daemon.sh bash -n x.sh",
		"GIT_EXTERNAL_DIFF=scripts/reinstall-daemon.sh git diff",
		"env BASH_ENV=scripts/reinstall-daemon.sh bash -n x.sh",
		"xargs -a scripts/reinstall-daemon.sh echo",
		"GIT_PAGER=claude git log",
		"EDITOR=/opt/bin/codex git commit",
		"GIT_PAGER='sh -c claude' git log",
		"export GIT_PAGER=claude; git log",
		"GIT_PAGER=gemini; git log",
		"GIT_PAGER=claude export GIT_PAGER; git log",
		"bash -n +n scripts/reinstall-daemon.sh",
		"bash -n +o noexec scripts/reinstall-daemon.sh",
		// Opus review (task-6e2bcd75).
		"cd ~/.local/bin && go build -o staypointd ./cmd/staypointd",
		"cd ~/.local && go build -o bin/staypoint ./cmd/staypoint",
		"cd ~/.local/bin && cp /tmp/x staypointd",
		"go build -C ~/.local/bin -o staypoint ./cmd/staypoint",
		"env -u STAYPOINT_TASK_ID GOFLAGS=-exec=/tmp/x.sh go test ./internal/x",
		"STAYPOINT_TASK_ID=other GOFLAGS=-toolexec=/tmp/x go vet ./...",
		"PATH=/tmp/evil:$PATH STAYPOINT_TASK_ID= go test ./x",
		"go(){ /tmp/x.sh; }; STAYPOINT_TASK_ID= go test ./x",
		"alias go=/tmp/x.sh; STAYPOINT_TASK_ID= go test ./x",
		"STAYPOINT_TASK_ID=other go test ./x",
		"env -u STAYPOINT_TASK_ID go test -overlay=/tmp/o.json ./x",
		"bash -O extglob -c 'env -u STAYPOINT_TASK_ID codex exec hi'",
		"bash --init-file /dev/null -c 'unset STAYPOINT_TASK_ID; staypoint task list'",
		"bash -O extglob -c 'claude -p hi'",
		"bash -O extglob -c 'cat x | sh'",
		"for v in TASK_ID SESSION_ID; do unset STAYPOINT_$v; done",
		"n=STAYPOINT_TASK_ID; unset $n",
		"if unset STAYPOINT_TASK_ID; then true; fi",
		"while env -u STAYPOINT_TASK_ID staypoint task list; do break; done",
		"until unset STAYPOINT_SESSION_ID; do :; done",
		"zsh -O -c 'unset STAYPOINT_TASK_ID'",
		"zsh +O -c 'claude -p x'",
		"zsh -no exec scripts/reinstall-daemon.sh",
		"bash -n +ox noexec scripts/reinstall-daemon.sh",
		"read GOFLAGS <<< -exec=/tmp/x.sh; export GOFLAGS; env -u STAYPOINT_TASK_ID go test ./x",
		". /tmp/envfile; env -u STAYPOINT_TASK_ID go test ./x",
		"source /tmp/envfile; env -u STAYPOINT_TASK_ID go test ./x",
		"hash -p /tmp/x.sh go; env -u STAYPOINT_TASK_ID go test ./x",
		"cd ~/.loc*/bin && go build -o staypointd ./cmd/staypointd",
		"exec 3>/tmp/r.sh; cat scripts/reinstall-daemon.sh >&3",
		"exec >/tmp/r.sh; cat scripts/reinstall-daemon.sh",
		"{ cat scripts/reinstall-daemon.sh; } > /tmp/r.sh",
		"( cat scripts/reinstall-daemon.sh ) > /tmp/r.sh",
		"for f in a; do cat scripts/reinstall-daemon.sh; done > /tmp/r.sh",
		"cd ~/.[l]ocal/bin && go build -o staypointd ./cmd/staypointd",
		"cd ~/{.local,x}/bin && cp /tmp/x staypointd",
		"zsh -n +O scripts/reinstall-daemon.sh",
		"cat scripts/reinstall-daemon.sh | cat > /tmp/r.sh",
		"git show HEAD:scripts/reinstall-daemon.sh | grep . > /tmp/r.sh",
		"git mv scripts/reinstall-daemon.sh scripts/r.sh && bash scripts/r.sh",
		"STAYPOINT_SKIP_PERMISSIONS=1 make",
		"zsh -n +o NO_EXEC scripts/reinstall-daemon.sh",
		"zsh -n -o exec scripts/reinstall-daemon.sh",
		// Final Opus review: git options that run a program, brace-expanded
		// gate variables, go test flags that run a linker or compiler.
		"git grep -O'scripts/reinstall-daemon.sh #' -e .",
		"git grep --open-files-in-pager='launchctl kickstart -k gui/501/com.staypoint.daemon #' -e .",
		"git grep --open-files-in-pager='true; claude -p hi' -e .",
		"git -C /r grep -nO'claude -p x #' -e .",
		"git grep -LoO'scripts/reinstall-daemon.sh #' -e .",
		"git grep -GO'scripts/reinstall-daemon.sh #' -e .",
		"git -c core.x=y grep -GO'claude -p x #' -e .",
		"git -c alias.g=grep g -nO'claude -p x #' -e .",
		"git --attr-source HEAD grep -nO'scripts/reinstall-daemon.sh #' -e .",
		"git --attr-source log grep -nO'claude -p x #' -e .",
		"git --frobnicate log grep -nO'scripts/reinstall-daemon.sh #' -e .",
		"git -c alias.d='!scripts/reinstall-daemon.sh' d",
		"git --exec-path=/tmp/x add scripts/reinstall-daemon.sh",
		"git fetch --upload-pack='scripts/reinstall-daemon.sh' origin",
		"git rebase -x scripts/reinstall-daemon.sh main",
		"git difftool -x 'claude -p' HEAD",
		"git difftool --extcmd='claude -p' HEAD",
		"git ls-remote -u 'claude -p' origin",
		"git diff --ext-diff -- scripts/reinstall-daemon.sh",
		"git show HEAD:scripts/reinstall-daemon.sh --output=/tmp/r.sh",
		"git grep --open='launchctl list #' -e .",
		"git grep --open-f 'claude -p x #' -e .",
		"git show --out=/tmp/r.sh HEAD:scripts/reinstall-daemon.sh",
		"git diff --ext -- scripts/reinstall-daemon.sh",
		"git grep -e -- -O'scripts/reinstall-daemon.sh #' .",
		"unset STAYPOINT_{TASK,SESSION}_ID",
		"unset STAY{POINT,X}_TASK_ID",
		"env -u 'ST*_TASK_ID' make",
		"env -u STAYPOINT_{TASK,SESSION}_ID make",
		"env -u STAYPOINT_TASK_ID go test -ldflags='-linkmode=external -extld=/tmp/x.sh' ./x",
		"env -u STAYPOINT_TASK_ID go test -gcflags=all=-N ./x",
		"env -u STAYPOINT_TASK_ID go test --ldflags -extld=/tmp/x.sh ./x",
		"env -u STAYPOINT_TASK_ID go test -compiler=gccgo -gccgoflags=-x ./x",
		// Board probe of bb36222: a filter fed the script that writes a file,
		// a kill pattern that matches the daemon, running a built daemon.
		`grep "" scripts/reinstall-daemon.sh | sort -o /tmp/r.sh; bash /tmp/r.sh`,
		"cat scripts/reinstall-daemon.sh | sort --output=/tmp/r.sh",
		"cat scripts/reinstall-daemon.sh | uniq - /tmp/r.sh",
		"cat scripts/reinstall-daemon.sh | sort -uo /tmp/r.sh",
		"cat scripts/reinstall-daemon.sh | sort --out /tmp/r.sh",
		"cat scripts/reinstall-daemon.sh | sort --compress-program=sh",
		"cat scripts/reinstall-daemon.sh | uniq -c /dev/stdin /tmp/r.sh",
		"pkill -f 'staypoint-apitest-server|staypointd'",
		"pkill -f 'stay.ointd'",
		"pkill -if STAYPOINTD",
		"killall -m 'stay.*'",
		"pkill -F /tmp/d.pid",
		`bash -c "pkill -f 'x|staypointd'"`,
		"echo staypointd | xargs pkill",
		"find . -name x -exec pkill -f 'a|staypointd' \\;",
		"pkill -f '('",
		"go build -o /tmp/staypointd ./cmd/staypointd && /tmp/staypointd",
		"go build -o /tmp/d ./cmd/staypointd && /tmp/d",
		"go build -o=/tmp/d ./cmd/staypointd; nohup /tmp/d &",
		"go build ./cmd/staypointd && ./staypointd",
		"go build -o /tmp/d ./cmd/staypointd && cp /tmp/d /tmp/e && /tmp/e",
		"go run ./cmd/staypointd",
		"/tmp/staypointd-x",
		"bash -c /tmp/staypointd",
		"find /tmp -name staypointd -exec {} \\;",
		`P=staypointd; pkill -f "$P"`,
		`D=/tmp/x; go build -o "$D" ./cmd/staypointd && "$D"`,
		"go build -o /tmp/d ./cmd/staypointd && sh -c /tmp/d",
		"go build -o /tmp/d ./cmd/staypointd && exec /tmp/d",
		"cat scripts/reinstall-daemon.sh | (cat > /tmp/r.sh)",
		"pkill -TERM staypointd",
		"pkill -HUP -f staypointd",
		"killall -STOP staypointd",
		"go build -o /tmp/d ./cmd/staypointd",
		"go build -o=/tmp/d ./cmd/staypointd",
		"cp /tmp/staypointd /tmp/e",
		"ln -s /tmp/staypointd-x /tmp/e",
		`python3 -c "import subprocess; subprocess.run(['/tmp/staypointd'])"`,
		"cd cmd/staypointd && go build -o /tmp/d .",
		"go -C cmd/staypointd build -o /tmp/d",
		"go -C=cmd/staypointd run .",
		"GOFLAGS=-o=/tmp/d go build ./cmd/staypointd",
		"cd cmd/staypointd && go run .",
		"env -C cmd/staypointd go build -o /tmp/d .",
		"env --chdir=cmd/staypointd go build -o /tmp/d .",
		"go --C cmd/staypointd build -o /tmp/d",
		"go --C=cmd/staypointd build -o /tmp/d",
		"go -x build -o /tmp/d ./cmd/staypointd",
		`cd "$D" && go build -o /tmp/d . # staypointd`,
		"cd cmd/stay* && go build -o /tmp/d . # staypointd",
		"go env -w GOFLAGS=-o=/tmp/d; go build ./cmd/staypointd",
		"cd cmd/staypointd && go build -o /tmp/d main.go",
		"cd cmd/staypointd && go build -o /tmp/d .//",
		"cd -P cmd/staypointd && go build -o /tmp/d",
		"cd -- cmd/staypointd && go build -race -o /tmp/d .",
		// A write that may carry what a command naming the script prints.
		"f(){ cat scripts/reinstall-daemon.sh; }; f > /tmp/r.sh",
		"cat <(cat scripts/reinstall-daemon.sh) > /tmp/r.sh",
		"if true; then cat scripts/reinstall-daemon.sh; fi > /tmp/r.sh",
		"cat scripts/reinstall-daemon.sh | while read l; do echo \"$l\"; done > /tmp/r.sh",
		"cat scripts/reinstall-daemon.sh | { cat > /tmp/r.sh; }",
		"cat scripts/reinstall-daemon.sh 2>/tmp/e | tee /tmp/r.sh",
		// The gate's env: unset or overridden for anything but go test/vet.
		"env -u STAYPOINT_TASK_ID claude -p x",
		"env -u STAYPOINT_TASK_ID staypoint task list",
		"env -u STAYPOINT_TASK_ID ./go test ./...",
		"env -u STAYPOINT_TASK_ID go test -exec ./x ./...",
		"env -u STAYPOINT_TASK_ID go test -toolexec=./x ./...",
		"env -u STAYPOINT_TASK_ID go vet -vettool=./x ./...",
		"env -u STAYPOINT_TASK_ID go run ./cmd/staypoint",
		"env -u STAYPOINT_TASK_ID go test ./... && env -u STAYPOINT_TASK_ID make",
		"env -uSTAYPOINT_TASK_ID bash",
		"env --unset=STAYPOINT_SESSION_ID bash",
		"env -i go test ./...",
		"STAYPOINT_TASK_ID=task-OTHER staypoint task comment x y",
		"STAYPOINT_SCRATCH_ROOT=/ go run .",
		"STAYPOINT_HOOK_BIN=/tmp/h make",
		"unset STAYPOINT_SESSION_ID",
		"export -n STAYPOINT_TASK_ID",
		"declare -x STAYPOINT_TASK_ID=x",
		"sh -c 'STAYPOINT_TASK_ID= staypoint task list'",
		// Nested agents still hold wherever they run.
		"watch 'ls | claude -p x'",
		"python3 <<EOF\nimport os; os.system('x; claude -p y')\nEOF",
		"bash -c 'echo hi; gemini -p x'",
		"echo $(codex exec x)",
		"pnpm dlx @google/gemini-cli -p x",
		"timeout 60 claude -p x",
		"echo claude -p x > /tmp/r.sh",
		"git -c alias.x='!claude -p y' x",
		"echo 'claude -p x' | sh",
	}
	for _, c := range held {
		why := AnalyzeBoardRulesForTask(task, c, nil)
		if why == "" {
			t.Errorf("not held: %q", c)
		}
	}

	allowed := []string{
		// Naming the reinstall script without running it (task-800b532d).
		"bash -n scripts/reinstall-daemon.sh",
		"bash -n scripts/reinstall-daemon.sh && echo ok",
		"sh -n scripts/reinstall-daemon.sh",
		"bash +x -n scripts/reinstall-daemon.sh",
		"bash +n -n scripts/reinstall-daemon.sh",
		"sed -ne '/launchctl/,/fi/p' scripts/reinstall-daemon.sh",
		"sed -n '1,40p;50q' scripts/reinstall-daemon.sh",
		"git add internal/security/boardrules.go scripts/reinstall-daemon.sh",
		"git add -- scripts/reinstall-daemon.sh && git commit -m 'fix: guard reinstall-daemon.sh deploys'",
		"git diff -- scripts/reinstall-daemon.sh",
		"git log -p scripts/reinstall-daemon.sh | head -50",
		"git show HEAD:scripts/reinstall-daemon.sh",
		"sed -n 1,40p scripts/reinstall-daemon.sh",
		"sed -n '/launchctl/p' scripts/reinstall-daemon.sh",
		"grep -n launchctl scripts/reinstall-daemon.sh",
		"grep -rn reinstall-daemon docs/ scripts/",
		"cat scripts/reinstall-daemon.sh",
		"head -20 scripts/reinstall-daemon.sh | wc -l",
		"wc -l scripts/reinstall-daemon.sh",
		"shellcheck scripts/reinstall-daemon.sh",
		"go vet ./... && bash -n scripts/reinstall-daemon.sh",
		"staypoint task comment T1 'ran bash -n scripts/reinstall-daemon.sh'",
		`gh pr create --title x --body "does not run scripts/reinstall-daemon.sh"`,
		"echo 'deploy with scripts/reinstall-daemon.sh after merge'",
		// Tests clear the task id to isolate themselves.
		"env -u STAYPOINT_TASK_ID go test ./cmd/staypoint/",
		"env -u STAYPOINT_TASK_ID go test -count=1 -run TestHook ./cmd/staypoint ./internal/...",
		"env -u STAYPOINT_TASK_ID go vet ./...",
		"STAYPOINT_TASK_ID= go test ./...",
		"nohup env -u STAYPOINT_TASK_ID go test ./x",
		"env -u STAYPOINT_TASK_ID -- go test ./x",
		"cd /r/.worktrees/x && env -u STAYPOINT_TASK_ID go test ./cmd/staypoint/ 2>&1 | tail -20",
		// Board 2026-10-09 false positives.
		"STAYPOINT_STYLE_AUDIT_DIR=/tmp/sa bash .worktrees/task-T1/scripts/style-audit.sh",
		"STAYPOINT_UI_STRICT=1 scripts/ui-e2e.sh specs/timeline.spec.ts",
		"go build -o /tmp/sa-e2e/staypoint-apitest-server ./cmd/staypoint-apitest-server",
		"go build -o /tmp/staypointd-x ./cmd/staypointd",
		"git add cmd/staypointd/*.go",
		"cat ~/.staypoint/handoffs/task-T1/latest.md",
		"cat $HOME/.staypoint/handoffs/task-T1/latest.md",
		"head -50 /Users/v/.staypoint/handoffs/task-T1/latest.md",
		"ls ~/.staypoint/handoffs/task-T1/",
		"cat ~/.staypoint/handoffs/task-T1/*.md 2>/dev/null",
		"pkill -f staypoint-apitest-server",
		"HOME=/tmp/h STAYPOINT_API_TOKEN=x STAYPOINT_BOARD_TOKEN=y /tmp/sa/staypoint-apitest-server",
		"local deadline=$(( $(date +%s) + 30 ))",
		// An agent name as data: grep patterns and test filters this run
		// was itself held on.
		"go test ./cmd/staypoint/ -run 'TestRaise|Hook|Gemini|Gate' -count=1",
		"grep -nE 'launchctl|claude|gemini|codex|agy|ssh' scripts/ui-e2e.sh",
		"rg -n 'x|claude|y' internal/",
		"git commit -m 'route x | gemini fallback'",
		"git log -S'TODO' scripts/reinstall-daemon.sh",
		"git log -G'FOO' -- scripts/reinstall-daemon.sh",
		// An O in a glued value is data (review of e3f6a20).
		`git commit -m"OOM fix by claude"`,
		"git log -L:Object:scripts/reinstall-daemon.sh",
		"git checkout -bOverhaul-claude",
		"git grep -e'Overnight claude' -- scripts/",
		"git grep -nA3 -e'Overnight' scripts/reinstall-daemon.sh",
		"cd internal/agy && go test ./...",
		"CLAUDE_CONFIG_DIR=~/.claude-work go test ./internal/adapter/",
		// A write that cannot carry the named command's output.
		"cat scripts/reinstall-daemon.sh; echo done > status.txt",
		"grep -c launchctl scripts/reinstall-daemon.sh && go test ./... > /tmp/t.log 2>&1",
		"bash -n scripts/reinstall-daemon.sh; go vet ./... > /tmp/vet.txt",
		"cat scripts/reinstall-daemon.sh | sort | uniq -c",
		"cat scripts/reinstall-daemon.sh | sort -u -k1,1",
		"cat scripts/reinstall-daemon.sh | uniq -c -",
		// Test servers and daemon builds that do not run the daemon.
		"pkill -f 'staypoint-apitest-server|vite'",
		"pkill -9 node",
		"go build -o /tmp/sa/ ./cmd/staypointd",
		"cp /tmp/staypointd /tmp/bin/",
		"git commit -m 'fix(staypointd): restart (dev)'",
		"go test ./cmd/staypointd/",
		"ls cmd/staypointd",
		"cd cmd/staypointd && go test ./...",
		"cd ~/dev/x && go test ./cmd/staypointd/",
		"go build -o /tmp/sa/staypoint-apitest-server ./cmd/staypoint-apitest-server && /tmp/sa/staypoint-apitest-server",
		"go build -o /tmp/staypointd ./cmd/staypointd && echo built",
		"git log --oneline -- cmd/staypointd",
	}
	for _, c := range allowed {
		if why := AnalyzeBoardRulesForTask(task, c, nil); why != "" {
			t.Errorf("held: %q: %s", c, why)
		}
	}
	// Without a task, no handoff dir is anyone's own.
	if AnalyzeBoardRules("cat ~/.staypoint/handoffs/task-T1/latest.md", nil) == "" {
		t.Error("handoff read held only by task")
	}

	// A script the command runs is judged by its contents: scripts/ui-e2e.sh
	// builds and kills its own test server and sets STAYPOINT_UI_* and test
	// tokens, none of which is self-protection.
	ui, err := os.ReadFile("../../scripts/ui-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	if why := AnalyzeBoardRulesForTask(task, "scripts/ui-e2e.sh specs/x.spec.ts",
		[]ScriptHash{{Path: "/r/scripts/ui-e2e.sh", Content: string(ui)}}); why != "" {
		t.Errorf("ui-e2e.sh held: %s", why)
	}
	for _, body := range []string{
		"#!/bin/sh\nbash -n scripts/reinstall-daemon.sh\n",
		"#!/bin/sh\nset -eu\ngo build -o \"$TMP/staypoint-apitest-server\" ./cmd/staypoint-apitest-server\nSTAYPOINT_API_TOKEN=\"$T\" \"$TMP/staypoint-apitest-server\" &\nkill \"$!\"\n",
	} {
		if why := AnalyzeBoardRulesForTask(task, "./run.sh", []ScriptHash{{Path: "/w/run.sh", Content: body}}); why != "" {
			t.Errorf("script %q held: %s", body, why)
		}
	}
	for _, body := range []string{
		"#!/bin/sh\ncat ~/.staypoint/auth_token\n",
		"#!/bin/sh\n  scripts/reinstall-daemon.sh\n",
		"#!/bin/sh\nlaunchctl kickstart -k gui/501/com.staypoint.daemon # restart\n",
		"#!/bin/sh\ncat <<EOF\n# x\nEOF\nsource ~/.staypoint/config.toml\n",
		"#!/bin/sh\n#cat ~/.staypoint/auth_token\nbash -c \"$(sed -n 's/^#//p' \"$0\")\"\n",
	} {
		why := AnalyzeBoardRulesForTask(task, "./run.sh", []ScriptHash{{Path: "/w/run.sh", Content: body}})
		if !strings.Contains(why, "self-protection") {
			t.Errorf("script %q not held for self-protection: %q", body, why)
		}
	}
}

// TestBoardRulesReplay re-judges held commands exported from the live gate
// table, which a task run may not read itself. The Board exports them with
//
//	sqlite3 -json ~/.staypoint/staypoint.db \
//	  "select task_id, cmdline, scripts_json from security_gate_requests where task_id='task-800b532d'" > /tmp/replay.json
//
// and runs SP_BOARDRULES_REPLAY=/tmp/replay.json go test ./internal/security/ -run Replay -v.
// Scripts are judged from the content the hook stored, which may be
// truncated; rows whose scripts have no stored content are counted, since
// their verdict covers the command line only. The test fails on any row
// still held for self-protection.
func TestBoardRulesReplay(t *testing.T) {
	path := os.Getenv("SP_BOARDRULES_REPLAY")
	if path == "" {
		t.Skip("SP_BOARDRULES_REPLAY not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		TaskID      string `json:"task_id"`
		Cmdline     string `json:"cmdline"`
		ScriptsJSON string `json:"scripts_json"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	other, blind := 0, 0
	for _, r := range rows {
		var scripts []ScriptHash
		if r.ScriptsJSON != "" {
			if err := json.Unmarshal([]byte(r.ScriptsJSON), &scripts); err != nil {
				t.Fatalf("scripts_json for %q: %v", r.Cmdline, err)
			}
		}
		for _, s := range scripts {
			if s.Content == "" {
				blind++
				break
			}
		}
		why := AnalyzeBoardRulesForTask(r.TaskID, r.Cmdline, scripts)
		switch {
		case strings.Contains(why, "self-protection"):
			t.Errorf("held: %q: %s", r.Cmdline, why)
		case why != "":
			other++
		}
	}
	t.Logf("%d rows: %d held by other Board rules, %d with scripts judged without content", len(rows), other, blind)
}
