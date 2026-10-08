#!/usr/bin/env python3
"""Typed-tools router eval (StayPoint task-4c0af644, parent task-3b4575f7).

Rule-based router: classifies a shell command string into the typed-tool
category a StayPoint MCP tool would own, plus the target environment.

Categories (most privileged first):
  staypoint_self, git_push, sql_write, ssh_write, http_write, open_pr,
  file_edit_script, other, bash_write, build_test, sql_read, ssh_read,
  http_read, bash_read

Compound commands are split into segments (&&, ||, ;, |, &, newline, plus
$(...) / backtick substitutions and heredoc bodies fed to interpreters);
each segment is classified and the record takes the most privileged one.

Read-only: opens ~/.staypoint/staypoint.db with mode=ro and never writes.
Output is masked (tokens, passwords, emails, IPs, long opaque strings).

Usage:
  python3 -I scripts/typed_tools_router_eval.py            # JSON report
  python3 -I scripts/typed_tools_router_eval.py --selftest # labelled cases
  python3 -I scripts/typed_tools_router_eval.py --classify 'cmd ...'
"""
import json
import os
import re
import shlex
import sqlite3
import sys
from collections import Counter, defaultdict

HOME = os.path.expanduser("~")
DB_PATH = os.path.join(HOME, ".staypoint", "staypoint.db")

RANK = {
    "staypoint_self": 100,
    "git_push": 90,
    "sql_write": 85,
    "ssh_write": 80,
    "http_write": 75,
    "open_pr": 70,
    "file_edit_script": 60,
    "other": 55,
    "bash_write": 50,
    "build_test": 40,
    "sql_read": 30,
    "ssh_read": 25,
    "http_read": 20,
    "bash_read": 10,
    "none": 0,
}
READ_CATS = {"sql_read", "ssh_read", "http_read", "bash_read"}
CATEGORIES = [c for c in RANK if c != "none"]

# ---------------------------------------------------------------- masking

_MASKS = [
    (re.compile(r"(?i)(Bearer\s+)\S+"), r"\1***"),
    (re.compile(r"(?i)\b([A-Z0-9_]*(?:PASSWORD|PASSWD|PWD|SECRET|TOKEN|API_?KEY|CLIENT_SECRET|AUTH)[A-Z0-9_]*)\s*=\s*(\"[^\"]*\"|'[^']*'|[^\s;&|]+)"), r"\1=***"),
    (re.compile(r"(?i)([?&](?:token|key|sig|code|access_token)=)[^&\s\"']+"), r"\1***"),
    (re.compile(r"(?i)(--?(?:password|passwd|token|secret|client-secret|api-key)[= ])\S+"), r"\1***"),
    (re.compile(r"(\s-P\s+)\S+"), r"\1***"),
    (re.compile(r"(\s-p)(?=[^\s-])\S+"), r"\1***"),
    (re.compile(r"(\s-u\s+)[^\s:]+:\S+"), r"\1***:***"),
    (re.compile(r"(://)[^/\s:@]+:[^/\s@]+@"), r"\1***:***@"),
    (re.compile(r"(?i)((?:Password|Pwd)=)[^;\"']+"), r"\1***"),
    (re.compile(r"eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]*"), "<jwt>"),
    (re.compile(r"[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}"), "<email>"),
    (re.compile(r"\b\d{1,3}(?:\.\d{1,3}){3}\b"), "<ip>"),
    (re.compile(r"(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b"), "<guid>"),
    (re.compile(r"\b(?=[A-Za-z0-9+_]*\d)(?=[A-Za-z0-9+_]*[g-zG-Z])[A-Za-z0-9+_]{32,}={0,2}"), "<redacted>"),
]


def mask(s, limit=170):
    s = s.replace("\r", " ").replace("\n", " \\n ")
    s = s.replace("127.0.0.1", "localhost").replace(HOME, "~")
    for rx, rep in _MASKS:
        s = rx.sub(rep, s)
    if limit and len(s) > limit:
        s = s[: limit - 1] + "…"
    return s


# ---------------------------------------------------------------- splitting


def split_segments(s):
    """Split a shell string into top-level segments.

    Returns (segments, unparseable). Each segment: dict(text, heredocs, piped)
    where piped means stdin came from the previous segment via '|'.
    """
    segs = []
    cur = []
    heredocs = []
    pending = []  # heredoc delimiters awaiting the next newline
    q = None
    depth = 0
    i = 0
    n = len(s)
    piped_next = False
    piped = False
    unparseable = False

    def flush(sep):
        nonlocal cur, heredocs, piped, piped_next
        text = "".join(cur).strip()
        if text or heredocs:
            segs.append({"text": text, "heredocs": heredocs, "piped": piped})
        cur = []
        heredocs = []
        piped = sep == "|"

    while i < n:
        c = s[i]
        if q == "'":
            cur.append(c)
            if c == "'":
                q = None
            i += 1
            continue
        if q == '"':
            cur.append(c)
            if c == "\\" and i + 1 < n:
                cur.append(s[i + 1])
                i += 2
                continue
            if c == '"':
                q = None
            i += 1
            continue
        if c == "\\" and i + 1 < n:
            if s[i + 1] == "\n":
                i += 2
                continue
            cur.append(c)
            cur.append(s[i + 1])
            i += 2
            continue
        if c in "'\"":
            q = c
            cur.append(c)
            i += 1
            continue
        if c == "#" and (i == 0 or s[i - 1] in " \t\n;&|(") and depth == 0:
            j = s.find("\n", i)
            i = n if j < 0 else j
            continue
        if s.startswith("$(", i):
            depth += 1
            cur.append("$(")
            i += 2
            continue
        if c == "(":
            if depth > 0:
                depth += 1
                cur.append(c)
            else:
                cur.append(" ")
            i += 1
            continue
        if c == ")":
            if depth > 0:
                depth -= 1
                cur.append(c)
            else:
                cur.append(" ")
            i += 1
            continue
        if s.startswith("<<", i) and not s.startswith("<<<", i):
            m = re.match(r"<<(-?)\s*(['\"]?)([A-Za-z0-9_.\-]+)\2", s[i:])
            if m:
                pending.append((m.group(3), m.group(1) == "-"))
                cur.append(m.group(0))
                i += len(m.group(0))
                continue
        if depth == 0:
            if c == "\n":
                if pending:
                    j = i + 1
                    for delim, strip in pending:
                        body = []
                        while j <= n:
                            k = s.find("\n", j)
                            line = s[j:] if k < 0 else s[j:k]
                            j = n + 1 if k < 0 else k + 1
                            if (line.lstrip("\t") if strip else line).strip() == delim:
                                break
                            body.append(line)
                        heredocs.append("\n".join(body))
                    pending = []
                    i = j
                    flush("\n")
                    continue
                flush("\n")
                i += 1
                continue
            if s.startswith("&&", i) or s.startswith("||", i):
                flush(s[i : i + 2])
                i += 2
                continue
            if c == ";":
                flush(";")
                i += 1
                continue
            if c == "|":
                flush("|")
                i += 1
                continue
            if c == "&":
                prev = s[i - 1] if i else ""
                nxt = s[i + 1] if i + 1 < n else ""
                if prev in "<>" or nxt == ">":
                    cur.append(c)
                    i += 1
                    continue
                flush("&")
                i += 1
                continue
        cur.append(c)
        i += 1
    if q or depth:
        unparseable = True
    if pending:
        unparseable = True
    flush("")
    return segs, unparseable


def substitutions(text, blank=False):
    """Extract $(...) and `...` bodies outside single quotes.

    With blank=True returns (bodies, text with each substitution replaced by
    the word __SUB__) so the outer command tokenizes cleanly."""
    out = []
    kept = []
    q = None
    i = 0
    n = len(text)
    while i < n:
        c = text[i]
        if q == "'":
            if c == "'":
                q = None
            kept.append(c)
            i += 1
            continue
        if c == "\\" and i + 1 < n:
            kept.append(text[i : i + 2])
            i += 2
            continue
        if c == "'" and q is None:
            q = "'"
            kept.append(c)
            i += 1
            continue
        if c == '"':
            q = None if q == '"' else '"'
            kept.append(c)
            i += 1
            continue
        if text.startswith("$(", i) and not text.startswith("$((", i):
            d = 1
            j = i + 2
            while j < n and d:
                if text[j] == "(":
                    d += 1
                elif text[j] == ")":
                    d -= 1
                j += 1
            out.append(text[i + 2 : j - 1])
            kept.append("__SUB__")
            i = j
            continue
        if c == "`":
            j = text.find("`", i + 1)
            if j < 0:
                kept.append(text[i:])
                break
            out.append(text[i + 1 : j])
            kept.append("__SUB__")
            i = j + 1
            continue
        kept.append(c)
        i += 1
    if blank:
        return out, "".join(kept)
    return out


def tokenize(text):
    try:
        lex = shlex.shlex(text, posix=True, punctuation_chars="<>&|;")
        lex.whitespace_split = True
        lex.commenters = ""
        return list(lex), False
    except ValueError:
        return text.split(), True


# ---------------------------------------------------------------- env

PROD_RX = re.compile(r"(?i)(?:^|[^a-z])(prod|production|prd)(?:[^a-z]|$)")
DEV_RX = re.compile(r"(?i)(?:^|[^a-z])(dev|development|staging|stage|test|qa|sandbox|uat)(?:[^a-z]|$)|dev-server")
LOCAL_RX = re.compile(r"(?i)\b(localhost|127\.0\.0\.1|::1|0\.0\.0\.0)\b")
# Assumption (unverified against inventory): bare ssh alias `mansol` is the prod box.
SSH_ENV = {"mansol": "prod", "mansol-dev": "dev", "mansol-mbp": "local", "mansol-sumit": "unknown",
           "mansol-datastream": "unknown", "home-windows-10": "local", "home-windows-10-wsl": "local"}


def env_of(target):
    if not target:
        return "unknown"
    if LOCAL_RX.search(target):
        return "local"
    if PROD_RX.search(target):
        return "prod"
    if DEV_RX.search(target):
        return "dev"
    return "unknown"


# ---------------------------------------------------------------- helpers

READ_CMDS = set("""
ls cat head tail grep egrep fgrep rg ag find sed awk gawk wc sort uniq cut tr jq yq echo printf pwd cd pushd popd which
command type stat file diff cmp test [ [[ true false sleep date env printenv ps lsof pgrep du df basename dirname
realpath readlink tree less more md5 shasum sha256sum base64 column nl comm uname whoami id hostname sw_vers
defaults plutil otool codesign strings xxd hexdump od mdfind mdls nslookup dig host ping nc netstat ifconfig
networksetup scutil sysctl vm_stat top uptime log man help export unset set source . read wait exit return
timedatectl hostnamectl journalctl free lsblk ss getent dpkg-query rpm apt-cache pip-list w last uname nproc
break continue break; sqlite_utils whereis locate gh-auth jq. az-version
local declare alias tput clear history time true : seq yes expr bc dc rev tac fold fmt paste join split csplit
iconv locale cal look  tsort xargs open code pbcopy pbpaste say osascript-read security-read gdate gstat
keychain ssh-add ssh-keygen-l go-doc lsappinfo pmset caffeinate
""".split())
WRITE_CMDS = set("""
rm rmdir mv cp mkdir touch chmod chown chgrp ln install truncate unlink rsync tar unzip gunzip gzip zip bzip2
xz 7z ditto kill killall pkill brew pip pip3 pipx uv npm-g gem cargo-install launchd crontab sqlite3-write
dd mkfifo mktemp patch xattr tmutil diskutil hdiutil screen tmux nohup
""".split())
BUILD_CMDS = set("""
make cmake ninja pytest tox nox cargo rustc tsc eslint prettier vitest jest playwright mocha ruff mypy black
isort flake8 pylint shellcheck shfmt golangci-lint staticcheck deadcode govulncheck gitleaks semgrep opengrep
xcodebuild swift swiftc gradle mvn dotnet bazel node-gyp hadolint actionlint yamllint markdownlint
""".split())
INTERP = {"python", "python3", "node", "ruby", "perl", "deno", "bun", "php", "osascript", "Rscript"}
SHELLS = {"bash", "sh", "zsh", "dash", "fish"}
SQL_CLIENTS = {"sqlcmd", "psql", "mysql", "mariadb", "sqlite3", "duckdb", "mongosh", "mongo", "redis-cli", "bcp", "clickhouse-client"}
NESTED_AGENTS = {"claude", "gemini", "agy", "codex", "aider", "cursor-agent"}
WRAPPERS = {"sudo", "timeout", "gtimeout", "nice", "nohup", "time", "command", "exec", "builtin", "caffeinate", "stdbuf", "unbuffer"}

SQL_WRITE_RX = re.compile(r"(?is)\b(insert\s+into|update\s+\S+\s+set|delete\s+from|merge\s+into|create\s+(table|index|view|schema|database|procedure|function|trigger|user|login|role)|alter\s+(table|database|index|user|login|role)|drop\s+(table|index|view|schema|database|procedure|function|trigger|user|login)|truncate\s+table|grant\s|revoke\s|exec(ute)?\s+(?!sp_help|sp_columns|sp_tables)|vacuum|reindex|replace\s+into|upsert|copy\s+\S+\s+from)\b")
SQL_READ_RX = re.compile(r"(?is)^\s*(select|with|show|describe|desc|explain|pragma|\.tables|\.schema|\.headers|\.mode|\.indexes|\.dump|\.read|\.timeout|set\s|declare\s|print\s|use\s|sp_help|sp_columns|begin\s+transaction\s+read\s+only|\\d|\\dt|\\l|\\x)")
FILES = {}  # path (and basename) -> heredoc body written earlier in the same command
FUNCS = set()  # shell functions defined in the same command (bodies classified where defined)
DJANGO_READ = {"showmigrations", "check", "diffsettings", "inspectdb", "sqlmigrate", "show_urls", "help", "version", "--version", "--help"}
SECRET_RX = re.compile(r"(?i)(az\s+keyvault\s+secret\s+(show|download)|az\s+account\s+get-access-token|security\s+find-(generic|internet)-password|op\s+(read|item\s+get)|gcloud\s+auth\s+print-access-token|vault\s+kv\s+get|aws\s+secretsmanager\s+get-secret-value|\.staypoint/auth_token|auth_token\b|board_token|\.env\b)")
STAYPOINT_PATH_RX = re.compile(r"(~|\$HOME|%s)/\.staypoint\b|\.staypoint/" % re.escape(HOME))
STAYPOINT_BIN_RX = re.compile(r"(go/bin|\.local/bin|/usr/local/bin)/staypoint")
SELF_MENTION_RX = re.compile(r"(?i)reinstall-daemon|launchctl|staypointd|\.staypoint\b|:41421|staypoint\.sock")

PY_WRITE_RX = re.compile(r"""(?s)(open\([^)]*['"][wax]b?\+?['"]|write_text|write_bytes|\.write\(|os\.remove|os\.unlink|\.unlink\(|shutil\.|os\.rename|\.rename\(|\.replace\(\s*['"]?[A-Za-z_]*\)|os\.makedirs|\.mkdir\(|json\.dump\(|\.save\(|\.delete\(\)|objects\.(?:create|bulk_create|get_or_create|update_or_create)|\)\.update\(|writeFileSync|writeFile\(|appendFile|fs\.rm|rmSync|renameSync|unlinkSync)""")
PY_HTTP_WRITE_RX = re.compile(r"""(?is)(requests\.(post|put|patch|delete)|method\s*=\s*['"](POST|PUT|PATCH|DELETE)|urlopen\([^)]*data\s*=|Request\([^)]*data\s*=|fetch\([^)]*method\s*:\s*['"](POST|PUT|PATCH|DELETE))""")
PY_HTTP_READ_RX = re.compile(r"(?i)(requests\.get|urlopen|urllib\.request|http\.client|fetch\()")
PY_SQL_RX = re.compile(r"(?i)(sqlite3\.connect|psycopg|pyodbc|pymssql|sqlalchemy|mysql\.connector)")
PY_PROC_RX = re.compile(r"(subprocess\.|os\.system|os\.popen|child_process|execSync|spawnSync)")


def in_tree(path, cwd):
    if not path:
        return True
    p = path.replace("$HOME", HOME)
    if p.startswith("~"):
        p = HOME + p[1:]
    if p.startswith(("/tmp", "/private/tmp", "/var/folders", "/dev/", "$TMPDIR", "${TMPDIR")):
        return True
    if not p.startswith("/"):
        return True
    if cwd:
        root = cwd
        m = re.match(r"(.*/\.worktrees/[^/]+)", cwd)
        if m:
            root = m.group(1)
        if p.startswith(root.rstrip("/") + "/") or p == root:
            return True
    return False


class Seg:
    def __init__(self, cat, env="local", target="", flags=None, writes=None, note=""):
        self.cat = cat
        self.env = env
        self.target = target
        self.flags = set(flags or ())
        self.writes = list(writes or ())
        self.note = note


def strip_prefix(tokens):
    flags = set()
    while tokens:
        t = tokens[0]
        if re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", t):
            if t.startswith("STAYPOINT_"):
                flags.add("staypoint_env")
            tokens = tokens[1:]
            continue
        if t in ("then", "do", "else", "elif", "if", "while", "until", "!", "{", "}", "done", "fi", "esac", "in"):
            tokens = tokens[1:]
            continue
        if t == "env":
            tokens = tokens[1:]
            while tokens and (tokens[0].startswith("-") or "=" in tokens[0]):
                if tokens[0] == "-u" and len(tokens) > 1:
                    if tokens[1].startswith("STAYPOINT_"):
                        flags.add("staypoint_env")
                    tokens = tokens[2:]
                    continue
                tokens = tokens[1:]
            continue
        if len(tokens) > 1 and tokens[1] == "{" and re.match(r"^[A-Za-z_][\w-]*$", t):
            tokens = tokens[2:]  # function definition: name() { body
            continue
        if t in WRAPPERS:
            if t == "command" and len(tokens) > 1 and tokens[1] in ("-v", "-V"):
                break
            tokens = tokens[1:]
            if t in ("timeout", "gtimeout"):
                while tokens and tokens[0].startswith("-"):
                    tokens = tokens[1:]
                if tokens and re.match(r"^\d+[smhd]?$", tokens[0]):
                    tokens = tokens[1:]
            elif t == "sudo":
                while tokens and tokens[0].startswith("-"):
                    if tokens[0] in ("-u", "-g") and len(tokens) > 1:
                        tokens = tokens[2:]
                    else:
                        tokens = tokens[1:]
            continue
        break
    return tokens, flags


def redirect_targets(tokens):
    out = []
    rest = []
    i = 0
    while i < len(tokens):
        t = tokens[i]
        if t in (">", ">>", ">|", "&>", "&>>", ">&") or re.match(r"^\d?>>?$", t):
            if i + 1 < len(tokens):
                tgt = tokens[i + 1]
                if not re.match(r"^\d$|^-$", tgt):
                    out.append(tgt)
            i += 2
            continue
        if t == "<" and i + 1 < len(tokens):
            STDIN_FILES.append(tokens[i + 1])
            i += 2
            continue
        if t == "<<<" and i + 1 < len(tokens):
            i += 2
            continue
        if re.match(r"^<<-?$", t):
            i += 2  # heredoc operator + delimiter word
            continue
        if re.match(r"^<<-?", t):
            i += 1
            continue
        rest.append(t)
        i += 1
    return out, rest


STDIN_FILES = []  # filled by redirect_targets for the segment being classified


def scratch(path):
    return path.startswith(("/dev/", "/tmp", "/private/tmp", "/var/folders", "$TMPDIR", "${TMPDIR")) or path in ("&1", "&2")


def classify_inline_code(code, lang="python"):
    flags = {"inline_script"}
    if STAYPOINT_PATH_RX.search(code) and PY_WRITE_RX.search(code):
        return Seg("staypoint_self", flags=flags, note="inline script writes ~/.staypoint")
    if PY_HTTP_WRITE_RX.search(code):
        tgt = " ".join(re.findall(r"https?://[^\s'\"]+", code))
        env = env_of(tgt) if tgt else "unknown"
        if ":41421" in tgt:
            return Seg("staypoint_self", "local", "staypoint-daemon", flags, note="inline script writes daemon API")
        return Seg("http_write", env, tgt, flags)
    if PY_SQL_RX.search(code):
        if SQL_WRITE_RX.search(code):
            return Seg("sql_write", "local", flags=flags)
        return Seg("sql_read", "local", flags=flags)
    if PY_WRITE_RX.search(code):
        paths = re.findall(r"""(?m)(?:open\(\s*|Path\(\s*|^\s*\w+\s*=\s*)['"]([\w./~\-]+\.\w{1,6})['"]""", code)
        return Seg("file_edit_script", flags=flags | {"heredoc_edit"}, writes=paths[:3])
    if PY_PROC_RX.search(code):
        return Seg("other", flags=flags | {"subprocess"})
    if PY_HTTP_READ_RX.search(code):
        tgt = " ".join(re.findall(r"https?://[^\s'\"]+", code))
        return Seg("http_read", env_of(tgt) if tgt else "unknown", tgt, flags)
    return Seg("bash_read", flags=flags)


def sql_mode(query):
    if query is None:
        return None
    query = re.sub(r"(?s)/\*.*?\*/", " ", query)
    query = re.sub(r"--[^\n]*", " ", query)
    if SQL_WRITE_RX.search(query):
        return "write"
    stmts = [x for x in re.split(r";|\bGO\b", query) if x.strip()]
    if stmts and all(SQL_READ_RX.match(x) for x in stmts):
        return "read"
    if SQL_READ_RX.match(query):
        return "read"
    return None


def classify_sql(cmd, args, heredocs, raw):
    query = None
    target = []
    readonly_flag = False
    i = 0
    pos = []
    while i < len(args):
        a = args[i]
        nxt = args[i + 1] if i + 1 < len(args) else ""
        if cmd == "sqlcmd":
            if a in ("-Q", "-q"):
                query = nxt; i += 2; continue
            if a in ("-S", "-d"):
                target.append(nxt); i += 2; continue
            if a == "-i":
                body = FILES.get(nxt) or FILES.get(os.path.basename(nxt))
                query = body if body is not None else query
                i += 2
                continue
            if a in ("-U", "-P", "-G", "--authentication-method", "-l", "-t", "-h", "-s", "-W", "-w", "-y", "-Y", "-o", "-K", "-N", "-C", "-b", "-k", "-v"):
                i += 2 if a not in ("-G", "-W", "-C", "-b", "-N", "-K") else 1
                continue
        elif cmd == "psql":
            if a in ("-c", "--command") or re.match(r"^-[a-zA-Z]*c$", a):
                query = nxt; i += 2; continue
            if a in ("-h", "-d", "--host", "--dbname"):
                target.append(nxt); i += 2; continue
            if a in ("-U", "-p", "-f", "-v", "-P"):
                i += 2; continue
        elif cmd in ("mysql", "mariadb"):
            if a in ("-e", "--execute"):
                query = nxt; i += 2; continue
            if a in ("-h", "-D", "--host", "--database"):
                target.append(nxt); i += 2; continue
        elif cmd == "sqlite3":
            if a == "-readonly":
                readonly_flag = True
            if a.startswith("-"):
                i += 1; continue
            pos.append(a); i += 1; continue
        if not a.startswith("-"):
            pos.append(a)
            if "://" in a:
                target.append(a)
        i += 1
    if cmd == "sqlite3":
        if pos:
            target.append(pos[0])
        if len(pos) > 1:
            query = " ".join(pos[1:])
    if query is None and heredocs:
        query = "\n".join(heredocs)
    tgt = " ".join(target)
    if cmd == "sqlite3" or cmd == "duckdb":
        env = "local"
    else:
        env = env_of(tgt) if tgt else "unknown"
    mode = "read" if readonly_flag else sql_mode(query)
    flags = set()
    if PROD_RX.search(raw) and mode == "read":
        flags.add("prod_read")
    if mode == "read":
        return Seg("sql_read", env, tgt, flags)
    if STAYPOINT_PATH_RX.search(tgt):
        return Seg("staypoint_self", "local", tgt, flags, note="writes staypoint db")
    flags.add("sql_mode_unknown" if mode is None else "sql_dml")
    return Seg("sql_write", env, tgt, flags)


def classify_curl(cmd, args):
    write = False
    get_flag = False
    urls = []
    i = 0
    while i < len(args):
        a = args[i]
        nxt = args[i + 1] if i + 1 < len(args) else ""
        if re.match(r"^-[a-zA-Z]{2,}$", a):  # clustered short flags, e.g. -sG, -sX POST
            if "G" in a:
                get_flag = True
            a = "-" + a[-1]
        if a in ("-X", "--request", "--method"):
            if nxt.upper() not in ("GET", "HEAD", "OPTIONS"):
                write = True
            i += 2
            continue
        m = re.match(r"^(?:-X|--request=|--method=)(\w+)$", a)
        if m:
            if m.group(1).upper() not in ("GET", "HEAD", "OPTIONS"):
                write = True
            i += 1
            continue
        if a in ("-G", "--get"):
            get_flag = True
        if a in ("-d", "--data", "--data-raw", "--data-binary", "--data-urlencode", "--json", "-F", "--form", "-T", "--upload-file", "--post-data", "--post-file", "--body-data"):
            write = True
            i += 2
            continue
        if re.match(r"^(--data[a-z\-]*|--json|--form|--post-data)=", a):
            write = True
        if re.match(r"^(https?://|localhost|127\.0\.0\.1)", a) or "://" in a:
            urls.append(a)
        i += 1
    if get_flag:
        write = False
    if cmd in ("http", "https", "xh"):
        if args and args[0].upper() in ("POST", "PUT", "PATCH", "DELETE"):
            write = True
    tgt = " ".join(urls)
    daemon = ":41421" in tgt or "staypoint.sock" in " ".join(args)
    if daemon:
        if write:
            return Seg("staypoint_self", "local", "staypoint-daemon", note="daemon API write")
        return Seg("http_read", "local", "staypoint-daemon", {"daemon_api"})
    env = env_of(tgt) if tgt else "unknown"
    if env == "unknown" and tgt:
        env = "external"
    return Seg("http_write" if write else "http_read", env, tgt)


def classify_gh(args):
    if not args:
        return Seg("other")
    sub = args[0]
    act = args[1] if len(args) > 1 else ""
    if sub == "pr":
        if act == "merge":
            return Seg("git_push", "external", "github", {"push_main", "gh_merge"})
        if act in ("create", "edit", "comment", "ready", "review", "reopen", "close", "lock"):
            return Seg("open_pr", "external", "github")
        return Seg("http_read", "external", "github")
    if sub == "api":
        method = None
        fields = False
        i = 1
        while i < len(args):
            a = args[i]
            if a in ("-X", "--method") and i + 1 < len(args):
                method = args[i + 1].upper(); i += 2; continue
            m = re.match(r"^(?:-X|--method=)(\w+)$", a)
            if m:
                method = m.group(1).upper()
            if a in ("-f", "-F", "--field", "--raw-field", "--input"):
                fields = True
            i += 1
        if method is None:
            method = "POST" if fields else "GET"
        if "graphql" in args and not any("mutation" in a for a in args):
            method = "GET"
        return Seg("http_write" if method not in ("GET", "HEAD") else "http_read", "external", "github")
    if sub in ("run", "workflow"):
        if act in ("rerun", "cancel", "run", "enable", "disable", "delete"):
            return Seg("http_write", "external", "github")
        return Seg("http_read", "external", "github")
    if sub == "auth":
        return Seg("bash_read", "local", flags={"secret_fetch"} if act == "token" else set())
    if act in ("create", "edit", "delete", "comment", "close", "reopen", "set", "upload", "transfer", "archive", "fork", "clone", "sync", "rename", "add", "remove"):
        if act in ("clone",):
            return Seg("bash_write", "local")
        return Seg("http_write", "external", "github")
    return Seg("http_read", "external", "github")


def classify_az(args, raw):
    words = [a for a in args if not a.startswith("-")]
    text = " ".join(args)
    flags = set()
    vals = " ".join(a for a in args)
    env = env_of(vals)
    if re.search(r"keyvault\s+secret\s+(show|download|list)", text) or re.search(r"account\s+get-access-token", text):
        flags.add("secret_fetch")
        return Seg("bash_read", env if env != "unknown" else "unknown", "keyvault", flags, note="secret fetch")
    if words[:1] in (["login"], ["logout"], ["account"]):
        return Seg("bash_read", "local")
    verbs_read = {"list", "show", "get", "query", "tail", "log", "download", "check-name", "exists", "list-keys", "browse", "wait", "version", "find"}
    verb = ""
    for w in words:
        if w in verbs_read or re.match(r"^(create|update|delete|set|restart|start|stop|deploy|purge|assign|add|remove|sync|invoke|import|upload|swap|reset|scale|config|up|run|apply|rotate|renew|recover|backup|restore)$", w):
            verb = w
    if verb in verbs_read or not verb:
        return Seg("http_read", env, "azure")
    return Seg("http_write", env, "azure")


def classify_ssh(cmd, args, heredocs, cwd, depth):
    opts_with_arg = set("-o -i -p -l -F -J -L -R -D -E -S -W -b -c -e -m -O -Q -w -B -P".split())
    if cmd in ("scp", "rsync", "sftp"):
        remote_args = [a for a in args if re.match(r"^[\w.\-]+@?[\w.\-]*:", a) and "://" not in a]
        if not remote_args:
            return Seg("bash_write", "local")
        pos = [a for a in args if not a.startswith("-")]
        host = remote_args[0].split(":")[0].split("@")[-1]
        env = SSH_ENV.get(host, env_of(host))
        if pos and pos[-1] in remote_args:
            return Seg("ssh_write", env, host, {"file_copy"})
        return Seg("ssh_read", env, host, {"file_copy"})
    i = 0
    host = None
    while i < len(args):
        a = args[i]
        if a in opts_with_arg:
            i += 2
            continue
        if a.startswith("-"):
            i += 1
            continue
        host = a
        i += 1
        break
    remote = args[i:]
    if host is None:
        return Seg("other")
    h = host.split("@")[-1]
    env = SSH_ENV.get(h, env_of(h))
    rcmd = " ".join(remote)
    if not rcmd and heredocs:
        rcmd = "\n".join(heredocs)
    if not rcmd:
        return Seg("ssh_write", env, h, {"interactive_shell"})
    sub = route(rcmd, cwd, depth + 1)
    if sub["cat"] == "other" and heredocs and rcmd != "\n".join(heredocs) and (
            sub["flags"] & {"interactive", "interactive_shell", "pipe_to_shell", "django_shell"}):
        # remote interpreter fed a local script on stdin: classify the script body
        if re.search(r"python|manage\.py", rcmd):
            s2 = classify_inline_code(heredocs[0])
            sub = {"cat": s2.cat, "flags": s2.flags | {"stdin_script"}}
        else:
            sub = route(heredocs[0], cwd, depth + 1)
            sub["flags"] = set(sub["flags"]) | {"stdin_script"}
    if sub["cat"] in READ_CATS or sub["cat"] == "none":
        return Seg("ssh_read", env, h, sub["flags"] & {"secret_fetch"})
    if sub["cat"] == "sql_write":
        return Seg("sql_write", env, h, {"via_ssh"})
    return Seg("ssh_write", env, h, {"remote_" + sub["cat"]})


def script_kind(path):
    b = os.path.basename(path)
    if b == "reinstall-daemon.sh" or "install-daemon" in b:
        return "staypoint_self"
    if b == "verify_dev_deploy.sh":
        return "ssh_read_dev"
    if re.search(r"(?i)(test|e2e|fuzz|check|lint|vet|bench|ci)", b):
        return "build_test"
    return "other"


def seg_from_script(path, args, cwd="", depth=0):
    body = FILES.get(path) or FILES.get(os.path.basename(path))
    if body is not None and depth < 4:
        if path.endswith(".py") or body.lstrip().startswith(("#!/usr/bin/env python", "import ", "from ")):
            s = classify_inline_code(body)
        else:
            r = route(body, cwd, depth + 1)
            s = Seg(r["cat"], r["env"], r["target"], set(r["flags"]), r["writes"])
        s.flags.add("script_written_same_cmd")
        return s
    k = script_kind(path)
    if k == "staypoint_self":
        return Seg("staypoint_self", "local", "reinstall-daemon", note="executes reinstall")
    if k == "ssh_read_dev":
        return Seg("ssh_read", "dev", "dev-server", {"verify_script"})
    if k == "build_test":
        return Seg("build_test")
    return Seg("other", flags={"opaque_script"})


def classify_segment(seg, cwd, depth):
    text = seg["text"]
    heredocs = list(seg["heredocs"])
    _, tok_text = substitutions(text, blank=True)
    tokens, bad = tokenize(tok_text)
    flags = set()
    if bad:
        flags.add("tokenize_fallback")
    del STDIN_FILES[:]
    redirs, tokens = redirect_targets(tokens)
    if heredocs and redirs:
        for r in redirs:
            FILES[r] = heredocs[0]
            FILES[os.path.basename(r)] = heredocs[0]
    if tokens and tokens[0] == "tee" and heredocs:
        for r in tokens[1:]:
            if not r.startswith("-"):
                FILES[r] = heredocs[0]
                FILES[os.path.basename(r)] = heredocs[0]
    for f in STDIN_FILES:
        body = FILES.get(f) or FILES.get(os.path.basename(f))
        if body is not None and not heredocs:
            heredocs = [body]
            flags.add("stdin_from_same_cmd_file")
    seg = dict(seg, heredocs=heredocs)
    tokens, pf = strip_prefix(tokens)
    flags |= pf
    writes = []
    redirect_seg = None
    for r in redirs:
        if scratch(r) or (not r.startswith(("/", "~", "$")) and scratch(seg.get("dir", ""))):
            continue
        writes.append(r)
        if STAYPOINT_PATH_RX.search(r):
            redirect_seg = Seg("staypoint_self", note="redirect into ~/.staypoint")
        elif redirect_seg is None:
            redirect_seg = Seg("file_edit_script", flags={"redirect_write"} | ({"heredoc_edit"} if heredocs else set()), writes=[r])
    if not tokens:
        base = Seg("bash_read") if not redirect_seg else redirect_seg
        base.flags |= flags
        return base
    cmd = os.path.basename(tokens[0])
    args = tokens[1:]
    s = classify_cmd(cmd, tokens[0], args, heredocs, text, seg, cwd, depth)
    s.flags |= flags
    if redirect_seg and RANK[redirect_seg.cat] > RANK[s.cat]:
        redirect_seg.flags |= s.flags
        redirect_seg.writes += s.writes
        s = redirect_seg
    if writes:
        s.writes += writes
    if SELF_MENTION_RX.search(text) and s.cat != "staypoint_self":
        s.flags.add("staypoint_mention")
    if SECRET_RX.search(text):
        s.flags.add("secret_fetch") if re.search(r"(?i)auth_token|keyvault|get-access-token|find-.*-password|op read|print-access-token|secretsmanager", text) else None
    return s


def classify_cmd(cmd, cmd0, args, heredocs, text, seg, cwd, depth):
    piped = seg["piped"]
    # Native tool gate entries ("Edit /path", "Write /path").
    if cmd in ("Edit", "Write", "MultiEdit", "NotebookEdit") and args:
        p = args[0]
        if STAYPOINT_PATH_RX.search(p):
            return Seg("staypoint_self", note="native edit of ~/.staypoint")
        return Seg("file_edit_script", flags={"native_tool"}, writes=[p])
    if cmd in ("Read", "Grep", "Glob"):
        return Seg("bash_read", flags={"native_tool"})
    if cmd in FUNCS:
        return Seg("bash_read", flags={"fn_call"})
    if cmd in ("cat", "head", "tail", "less", "more", "xxd", "base64", "strings", "cp", "pbcopy") and re.search(r"\.staypoint/(auth_token|board)", text):
        # printing / copying the StayPoint token itself (inside $(...) it is downgraded in route)
        return Seg("staypoint_self", "local", "auth_token", {"secret_fetch"}, note="token_read")
    if cmd == "__SUB__" or cmd.startswith("$"):
        return Seg("other", flags={"indirect_exec"})
    if cmd == "crontab":
        return Seg("bash_read" if "-l" in args else "bash_write")
    if re.match(r"^(opengrep|semgrep|gitleaks|golangci-lint)", cmd):
        return Seg("build_test")
    if cmd in INTERP or re.match(r"^python3?(\.\d+)?$", cmd):
        pos = [a for a in args if not a.startswith("-")]
        if pos and os.path.basename(pos[0]) == "manage.py":
            sub = pos[1] if len(pos) > 1 else ""
            if sub in DJANGO_READ:
                return Seg("bash_read", flags={"django"})
            if sub == "test":
                return Seg("build_test", flags={"django"})
            if sub in ("shell", "dbshell", "shell_plus"):
                if "-c" in args or "--command" in args:
                    k = args.index("-c") if "-c" in args else args.index("--command")
                    s = classify_inline_code(args[k + 1] if k + 1 < len(args) else "")
                    s.flags.add("django")
                    return s
                if heredocs:
                    s = classify_inline_code(heredocs[0])
                    s.flags.add("django")
                    return s
                return Seg("other", flags={"django_shell", "interactive"})
            if sub == "makemigrations":
                return Seg("file_edit_script", flags={"django"})
            return Seg("sql_write", "unknown", "django:" + sub, {"django"})
    if cmd0.endswith(".sh") or (cmd0.startswith(("./", "scripts/", "/")) and "/" in cmd0 and cmd not in READ_CMDS and cmd not in BUILD_CMDS and cmd not in WRITE_CMDS and cmd not in SHELLS and cmd not in INTERP and cmd != "git" and cmd not in SQL_CLIENTS and not cmd.startswith("staypoint")):
        return seg_from_script(cmd0, args, cwd, depth)
    if cmd == "git":
        return classify_git(args, text)
    if cmd == "gh":
        return classify_gh(args)
    if cmd in ("curl", "wget", "http", "https", "xh"):
        return classify_curl(cmd, args)
    if cmd in ("ssh", "scp", "rsync", "sftp", "mosh"):
        return classify_ssh(cmd, args, heredocs, cwd, depth)
    if cmd in SQL_CLIENTS:
        return classify_sql(cmd, args, heredocs, text)
    if cmd == "az":
        return classify_az(args, text)
    if cmd in ("aws", "gcloud", "kubectl", "terraform", "wrangler", "vercel", "supabase", "flyctl", "heroku", "doctl", "pulumi"):
        verb_w = re.search(r"\b(apply|create|delete|deploy|put|set|update|rm|destroy|push|scale|rollout|patch|exec|edit|cp|sync|invoke|publish|import)\b", " ".join(args))
        env = env_of(" ".join(args))
        return Seg("http_write" if verb_w else "http_read", env if env != "unknown" else "external", cmd)
    if cmd == "launchctl":
        sub = args[0] if args else ""
        if sub in ("list", "print", "blame", "getenv", "print-disabled", "managername", "help", "error", "dumpstate"):
            return Seg("bash_read", flags={"staypoint_mention"} if "staypoint" in text else set())
        if "staypoint" in text.lower() or "41421" in text:
            return Seg("staypoint_self", note="launchctl on staypoint")
        return Seg("bash_write")
    if cmd in ("staypointd",):
        if any(a in ("--help", "-h", "version", "--version") for a in args):
            return Seg("bash_read")
        return Seg("staypoint_self", note="runs staypointd")
    if cmd == "staypoint":
        a = " ".join(args)
        words = [w for w in args if not w.startswith("-")]
        action = words[1] if len(words) > 1 else (words[0] if words else "")
        if not args or re.search(r"(^|\s)(--help|-h|help|version|--version|status)(\s|$)", a) or action in (
                "list", "get", "show", "view", "search", "lookup", "status", "log", "logs", "interactions", "diff", "cat") or (
                len(words) > 2 and words[2] in ("list", "get", "show", "view")):
            return Seg("bash_read", "local", "staypoint-cli")
        if re.match(r"^(daemon|install|reinstall|serve|web|trust|gate\s+(approve|deny))", a):
            return Seg("staypoint_self", note="staypoint cli self op")
        return Seg("open_pr" if False else "http_write", "local", "staypoint-cli", {"staypoint_cli_write"})
    if cmd in ("pkill", "killall", "kill"):
        if "staypoint" in text:
            return Seg("staypoint_self", note="kills staypointd")
        return Seg("bash_write")
    if cmd in NESTED_AGENTS:
        if any(a in ("--version", "-v", "--help", "-h") for a in args):
            return Seg("bash_read")
        return Seg("other", flags={"nested_agent"})
    if cmd == "go":
        sub = args[0] if args else ""
        if sub in ("doc", "list", "env", "version", "help", "tool"):
            return Seg("bash_read")
        if sub == "install" or (sub == "build" and STAYPOINT_BIN_RX.search(text)):
            if "staypoint" in text:
                return Seg("staypoint_self", note="installs staypoint binary")
            return Seg("bash_write")
        if sub in ("build", "test", "vet", "run", "generate", "mod", "fmt", "fix", "work", "get", "clean"):
            return Seg("build_test")
        return Seg("build_test")
    if cmd == "gofmt":
        return Seg("file_edit_script" if "-w" in args else "build_test")
    if cmd in ("npm", "pnpm", "yarn", "bun", "npx", "bunx", "pnpx"):
        sub = args[0] if args else ""
        if sub in ("view", "ls", "list", "info", "outdated", "why", "config", "help", "-v", "--version", "bin", "root", "prefix"):
            return Seg("bash_read")
        if sub in ("publish", "deprecate", "unpublish"):
            return Seg("http_write", "external", "npm")
        if cmd in ("bun",) and sub and (sub.endswith((".ts", ".js")) or sub == "-e"):
            return Seg("other", flags={"opaque_script"})
        return Seg("build_test")
    if cmd in BUILD_CMDS:
        if cmd in ("prettier", "black", "isort", "ruff") and any(a in ("--write", "-w", "format", "--fix") for a in args) and cmd != "black":
            return Seg("build_test", flags={"formatter_write"})
        return Seg("build_test")
    if cmd in ("systemctl", "service", "journalctl", "launchd"):
        if cmd == "journalctl":
            return Seg("bash_read")
        sub = next((a for a in args if not a.startswith("-")), "")
        if sub in ("status", "is-active", "is-enabled", "is-failed", "show", "cat", "list-units", "list-unit-files",
                   "list-timers", "list-sockets", "list-dependencies", "get-default", "help"):
            return Seg("bash_read")
        return Seg("bash_write", flags={"service_control"})
    if cmd == "docker":
        sub = args[0] if args else ""
        if sub in ("build", "buildx"):
            return Seg("build_test")
        if sub in ("ps", "images", "logs", "inspect", "version", "info", "stats", "top", "port", "history"):
            return Seg("bash_read")
        return Seg("bash_write")
    if cmd in SHELLS:
        if "-n" in args:
            return Seg("bash_read", flags={"syntax_check"})
        if "-c" in args:
            k = args.index("-c")
            if k + 1 < len(args):
                sub = route(args[k + 1], cwd, depth + 1)
                return Seg(sub["cat"], sub["env"], sub["target"], set(sub["flags"]))
        scripts_ = [a for a in args if not a.startswith("-")]
        if "-s" in args:
            scripts_ = []
        if scripts_:
            return seg_from_script(scripts_[0], args, cwd, depth)
        if heredocs:
            sub = route("\n".join(heredocs), cwd, depth + 1)
            return Seg(sub["cat"], sub["env"], sub["target"], set(sub["flags"]) | {"heredoc_script"})
        if piped:
            if "verify_dev_deploy.sh" in seg.get("prev", ""):
                return Seg("ssh_read", "dev", "dev-server", {"verify_script", "pipe_to_shell"})
            return Seg("other", flags={"pipe_to_shell"})
        return Seg("other", flags={"interactive_shell"})
    if cmd in INTERP or re.match(r"^python3?(\.\d+)?$", cmd):
        code = None
        if "-c" in args or "-e" in args:
            k = args.index("-c") if "-c" in args else args.index("-e")
            code = args[k + 1] if k + 1 < len(args) else ""
        elif cmd == "perl" and any(a.startswith("-") and "i" in a and "e" in a for a in args):
            return Seg("file_edit_script", flags={"inplace_edit"}, writes=[a for a in args if not a.startswith("-")][-1:])
        elif "-" in args or (heredocs and not [a for a in args if not a.startswith("-")]):
            code = "\n".join(heredocs)
        elif "-m" in args:
            k = args.index("-m")
            mod = args[k + 1] if k + 1 < len(args) else ""
            if mod in ("pytest", "unittest", "py_compile", "compileall", "mypy", "ruff", "black", "pip"):
                return Seg("build_test")
            if mod in ("json.tool", "http.server", "this", "site", "pydoc", "base64"):
                return Seg("bash_read")
            return Seg("other", flags={"opaque_script"})
        if code is None:
            pos = [a for a in args if not a.startswith("-")]
            if pos:
                return seg_from_script(pos[0], args, cwd, depth)
            if piped:
                return Seg("bash_read", flags={"pipe_filter"})
            return Seg("other", flags={"interactive"})
        if cmd == "osascript":
            return Seg("other", flags={"osascript"})
        s = classify_inline_code(code)
        if heredocs:
            s.flags.add("heredoc")
        return s
    if cmd == "sed":
        inplace = any(a == "-i" or a.startswith("-i") or a == "--in-place" for a in args)
        if inplace:
            files = [a for a in args if not a.startswith("-") and ("/" in a or "." in a)]
            if any(STAYPOINT_PATH_RX.search(f) for f in files):
                return Seg("staypoint_self", note="sed -i on ~/.staypoint")
            return Seg("file_edit_script", flags={"inplace_edit"}, writes=files[-1:])
        return Seg("bash_read")
    if cmd == "awk" and "-i" in args:
        return Seg("file_edit_script", flags={"inplace_edit"})
    if cmd == "tee":
        files = [a for a in args if not a.startswith("-")]
        real = [f for f in files if not scratch(f)]
        if any(STAYPOINT_PATH_RX.search(f) for f in real):
            return Seg("staypoint_self")
        if real:
            return Seg("file_edit_script", flags={"redirect_write"} | ({"heredoc_edit"} if heredocs else set()), writes=real)
        return Seg("bash_read")
    if cmd == "cat" and heredocs:
        return Seg("bash_read", flags={"heredoc"})
    if cmd == "find":
        if "-delete" in args or ("-exec" in args and re.search(r"-exec\s+(rm|mv|sed\s+-i|chmod)", text)):
            return Seg("bash_write")
        return Seg("bash_read")
    if cmd == "xargs":
        rest = [a for a in args if not a.startswith("-")]
        if rest:
            return classify_cmd(os.path.basename(rest[0]), rest[0], rest[1:], [], " ".join(rest), seg, cwd, depth)
        return Seg("bash_read")
    if cmd in ("security",):
        if args and args[0].startswith("find-"):
            return Seg("bash_read", flags={"secret_fetch"})
        return Seg("bash_write")
    if cmd in ("defaults", "plutil") and args and args[0] in ("write", "delete", "-replace", "-insert", "-remove", "-convert"):
        return Seg("bash_write")
    if cmd == "patch":
        return Seg("file_edit_script", flags={"patch"})
    if cmd in WRITE_CMDS or cmd in ("cp", "mv", "rm", "ln", "install", "mkdir", "touch", "chmod"):
        pos = [a for a in args if not a.startswith("-")]
        dest = pos[-1:] if cmd in ("cp", "mv", "ln", "install", "rsync", "ditto") else pos
        if any(STAYPOINT_PATH_RX.search(p) or STAYPOINT_BIN_RX.search(p) for p in dest):
            return Seg("staypoint_self", note="%s into ~/.staypoint or staypoint bin" % cmd)
        fl = set()
        if cmd == "rm" and any(re.match(r"^-[a-zA-Z]*r", a) for a in args):
            fl.add("rm_recursive")
        return Seg("bash_write", flags=fl, writes=[p for p in dest if not scratch(p)])
    if cmd in READ_CMDS:
        if cmd == "osascript":
            return Seg("other")
        return Seg("bash_read")
    if cmd in ("for", "case", "select", "function", "declare", "trap", "shift", "getopts", "printf", "let", "eval"):
        if cmd == "eval":
            return Seg("other", flags={"eval"})
        return Seg("bash_read")
    return Seg("other", flags={"unknown_cmd:" + cmd[:24]})


def classify_git(args, text):
    i = 0
    while i < len(args) and args[i].startswith("-"):
        if args[i] in ("-C", "-c", "--git-dir", "--work-tree"):
            i += 2
        else:
            i += 1
    sub = args[i] if i < len(args) else ""
    rest = args[i + 1 :]
    if sub == "push":
        refs = [a for a in rest if not a.startswith("-")]
        main = any(re.search(r"(^|[:/])(main|master)$", r) for r in refs[1:]) or any(r in ("main", "master") for r in refs)
        fl = {"push_main"} if main else set()
        if any(a in ("-f", "--force", "--force-with-lease") or a.startswith("--force") for a in rest):
            fl.add("force_push")
        if "--dry-run" in rest or "-n" in rest:
            return Seg("bash_read", "external", "github", fl)
        return Seg("git_push", "external", "github", fl)
    read_subs = {"status", "log", "diff", "show", "fetch", "rev-parse", "ls-files", "ls-remote", "grep", "blame", "merge-base",
                 "check-ignore", "describe", "shortlog", "cat-file", "rev-list", "reflog", "name-rev", "for-each-ref",
                 "symbolic-ref", "count-objects", "help", "version", "var", "whatchanged", "range-diff", "diff-tree",
                 "show-ref", "verify-commit", "check-attr", "ls-tree", "show-branch", "cherry", "difftool", "annotate",
                 "--version", "remote-show", "fsck", "notes"}
    if sub in read_subs:
        return Seg("bash_read", "local", "git")
    if sub == "branch":
        if any(a in ("-d", "-D", "-m", "-M", "-f", "--delete", "--move", "-c", "-C", "--set-upstream-to", "-u") for a in rest):
            return Seg("bash_write", "local", "git")
        pos = [a for a in rest if not a.startswith("-")]
        if pos and not any(a in ("--list", "-l", "-a", "-r", "--all", "--contains", "--merged", "--no-merged", "--show-current", "-v", "-vv") for a in rest):
            return Seg("bash_write", "local", "git")
        return Seg("bash_read", "local", "git")
    if sub == "remote":
        if rest and rest[0] in ("add", "remove", "rm", "set-url", "rename", "prune", "set-head"):
            return Seg("bash_write", "local", "git")
        return Seg("bash_read", "local", "git")
    if sub == "config":
        if any(a in ("--get", "-l", "--list", "--get-all", "--get-regexp", "--show-origin") for a in rest) or len([a for a in rest if not a.startswith("-")]) <= 1:
            return Seg("bash_read", "local", "git")
        return Seg("bash_write", "local", "git")
    if sub in ("worktree", "stash"):
        if rest and rest[0] in ("list", "show"):
            return Seg("bash_read", "local", "git")
        return Seg("bash_write", "local", "git")
    if sub == "apply" and "--check" in rest:
        return Seg("bash_read", "local", "git")
    if sub == "apply":
        return Seg("file_edit_script", "local", "git", {"patch"})
    fl = set()
    if sub == "reset" and "--hard" in rest:
        fl.add("reset_hard")
    return Seg("bash_write", "local", "git", fl)


def route(cmdline, cwd="", depth=0):
    """Classify a full command line. Returns dict."""
    if depth > 4:
        return {"cat": "other", "env": "unknown", "target": "", "flags": {"too_deep"}, "segments": [], "writes": []}
    if depth == 0:
        FILES.clear()
        FUNCS.clear()
    FUNCS.update(re.findall(r"(?m)(?:^|[;&|\s])([A-Za-z_][\w-]*)\s*\(\)\s*\{", cmdline))
    segs, unparseable = split_segments(cmdline)
    results = []
    prev = ""
    cur_dir = ""
    for seg in segs:
        m = re.match(r"^\s*cd\s+(\S+)", seg["text"])
        if m:
            cur_dir = m.group(1).strip("'\"")
        seg["dir"] = cur_dir
        seg["prev"] = prev
        prev = seg["text"]
        s = classify_segment(seg, cwd, depth)
        results.append(s)
        for sub in substitutions(seg["text"]):
            r = route(sub, cwd, depth + 1)
            ss = Seg(r["cat"], r["env"], r["target"], set(r["flags"]) | {"substitution"}, r["writes"])
            if r.get("note") == "token_read":
                ss = Seg("bash_read", "local", "auth_token", ss.flags | {"secret_fetch"})
            if re.search(r"(?i)keyvault|get-access-token|auth_token|find-generic-password|op read", sub):
                ss.flags.add("secret_fetch")
            results.append(ss)
    flags = set()
    writes = []
    for r in results:
        flags |= r.flags
        writes += r.writes
    if unparseable:
        flags.add("unparseable")
    if any(h for sg in segs for h in sg["heredocs"]):
        flags.add("heredoc")
    if not results:
        return {"cat": "none", "env": "local", "target": "", "flags": flags, "segments": [], "writes": writes}
    top = max(results, key=lambda r: RANK[r.cat])
    env = top.env
    if any(r.env == "prod" and r.cat not in READ_CATS for r in results):
        flags.add("prod_write_segment")
    if any(r.env == "prod" for r in results):
        flags.add("prod_target")
    return {
        "cat": top.cat,
        "env": env,
        "target": top.target,
        "flags": flags,
        "segments": [(r.cat, r.env) for r in results],
        "writes": writes,
        "note": top.note,
    }


FAMILY = {"git_push": "git", "open_pr": "git", "sql_read": "sql", "sql_write": "sql", "ssh_read": "ssh",
          "ssh_write": "ssh", "http_read": "http", "http_write": "http"}


def family(rec):
    """Typed-tool family that would own the command (local git writes count as git)."""
    if rec["cat"] == "bash_write" and rec.get("target") == "git":
        return "git"
    return FAMILY.get(rec["cat"], rec["cat"])


def bucket(rec, cwd):
    """A = typed read tool auto-allows; B = non-prod write auto-allowed by trust;
    C = genuinely Board; D = opaque, needs review/rewrite."""
    c, env, fl = rec["cat"], rec["env"], rec["flags"]
    outside = [w for w in rec["writes"] if not in_tree(w, cwd)]
    if c in READ_CATS:
        return "A"
    if c == "git_push":
        return "C" if "push_main" in fl else "B"
    if c == "staypoint_self":
        return "C"
    if c in ("sql_write", "ssh_write", "http_write"):
        if env == "prod" or "prod_write_segment" in fl:
            return "C"
        if env in ("dev", "local"):
            return "B"
        return "C"
    if c == "open_pr":
        return "B"
    if c in ("build_test", "file_edit_script", "bash_write"):
        if "prod_write_segment" in fl:
            return "C"
        return "D" if outside else "B"
    return "D"


# ---------------------------------------------------------------- reasons

REASON_CLASSES = [
    ("unparseable / heredoc", lambda r, c: "unparseable" in r or ("opaque script" in r and "<<" in c)),
    ("self-protect rule (StayPoint/guards)", lambda r, c: "StayPoint or its guards" in r),
    ("sensitive path ~/.staypoint", lambda r, c: "sensitive path" in r and ".staypoint" in r),
    ("token file read", lambda r, c: "auth/board token" in r),
    ("opaque script (bash/python runs a file)", lambda r, c: "opaque script" in r and "<<" not in c),
    ("script env/size/mutation", lambda r, c: re.search(r"changed environment|too large|may be changed", r) is not None),
    ("ssh remote shell", lambda r, c: "ssh: remote shell" in r),
    ("prod-write rule", lambda r, c: "may write to prod" in r),
    ("indirect $VAR/$(...)/eval", lambda r, c: "indirect command" in r),
    ("nested agent / StayPoint env", lambda r, c: "nested agent" in r),
    ("push to main", lambda r, c: "push targets main" in r),
    ("external API write", lambda r, c: "external API" in r),
    ("edit outside worktree", lambda r, c: "outside the task worktree" in r),
    ("rm -r / reset --hard", lambda r, c: "rm with recursive" in r or "reset --hard" in r),
    ("pipe into shell", lambda r, c: "piping into a shell" in r or "interactive or opaque" in r),
]


def reason_classes(reasons, cmd):
    joined = " | ".join(reasons)
    out = [name for name, fn in REASON_CLASSES if fn(joined, cmd)]
    return out or ["other: " + mask(joined, 60)]


# ---------------------------------------------------------------- selftest

SELFTEST = [
    ("cat > internal/x.go <<'EOF'\npackage x\nEOF", "file_edit_script", "local"),
    ("python3 - <<'EOF'\np='a.go'\ns=open(p).read()\nopen(p,'w').write(s)\nEOF", "file_edit_script", "local"),
    ("bash -n scripts/reinstall-daemon.sh", "bash_read", "local"),
    ("git add scripts/reinstall-daemon.sh && git commit -m 'fix reinstall'", "bash_write", "local"),
    ("scripts/reinstall-daemon.sh", "staypoint_self", "local"),
    ("launchctl kickstart -k gui/501/com.staypoint.daemon", "staypoint_self", "local"),
    ("launchctl list | grep staypoint", "bash_read", "local"),
    ("cd /tmp && timeout 60 sqlcmd -S sql-x-prod-eastus.database.windows.net -d db-prod -Q \"SELECT TOP 5 * FROM t\"", "sql_read", "prod"),
    ("sqlcmd -S sql-x-prod.database.windows.net -Q \"UPDATE t SET a=1\"", "sql_write", "prod"),
    ("sqlite3 -readonly ~/.staypoint/staypoint.db 'select 1'", "sql_read", "local"),
    ("export SQLCMDPASSWORD=$(az account get-access-token --query accessToken -o tsv); sqlcmd -S s-prod -Q 'SELECT 1'", "sql_read", "prod"),
    ("ssh mansol-dev 'systemctl is-active foo.service; journalctl -u foo --no-pager | tail'", "ssh_read", "dev"),
    ("ssh mansol-dev 'sudo systemctl restart foo'", "ssh_write", "dev"),
    ("ssh mansol", "ssh_write", "prod"),
    ("curl -s http://localhost:41421/api/tasks/x", "http_read", "local"),
    ("curl -s -X POST http://127.0.0.1:41421/api/tasks/x/comments -d @f.json", "staypoint_self", "local"),
    ("curl -sS https://example.com/docs.md", "http_read", "external"),
    ("curl -X POST https://api.example.com/v1/thing -d '{}'", "http_write", "external"),
    ("gh pr create --base main --title t --body b", "open_pr", "external"),
    ("gh pr merge 12 --squash", "git_push", "external"),
    ("gh api repos/o/r/pulls/1", "http_read", "external"),
    ("git push origin HEAD:main", "git_push", "external"),
    ("git push -u origin staypoint/task-x", "git_push", "external"),
    ("go build ./... && go vet ./... && go test ./...", "build_test", "local"),
    ("env -u STAYPOINT_TASK_ID go test ./cmd/staypoint", "build_test", "local"),
    ("git log --oneline | grep -i agy | head", "bash_read", "local"),
    ("sed -i '' 's/a/b/' internal/x.go", "file_edit_script", "local"),
    ("git show origin/dev-server:scripts/verify_dev_deploy.sh | bash -s -- abc /x=y", "ssh_read", "dev"),
    ("curl -s https://x.sh/install | bash", "other", "local"),
    ("az keyvault secret show --vault-name kv-app-prod --name x --query value -o tsv", "bash_read", "prod"),
    ("Edit /Users/someone/.claude/projects/x/memory/MEMORY.md", "file_edit_script", "local"),
    ("cp staypoint ~/go/bin/staypoint", "staypoint_self", "local"),
    ("rm -rf /tmp/foo", "bash_write", "local"),
]


def selftest():
    bad = 0
    for cmd, cat, env in SELFTEST:
        r = route(cmd, "/repo/.worktrees/task-x")
        ok = r["cat"] == cat and r["env"] == env
        if not ok:
            bad += 1
        print("%s  %-16s %-8s  want %-16s %-8s  %s" % ("ok " if ok else "BAD", r["cat"], r["env"], cat, env, mask(cmd, 80)))
    print("%d/%d pass" % (len(SELFTEST) - bad, len(SELFTEST)))
    return bad


# ---------------------------------------------------------------- main


def pct(a, b):
    return round(100.0 * a / b, 1) if b else 0.0


def main():
    if "--selftest" in sys.argv:
        sys.exit(1 if selftest() else 0)
    if "--classify" in sys.argv:
        cmd = sys.argv[sys.argv.index("--classify") + 1]
        r = route(cmd, os.getcwd())
        r["flags"] = sorted(r["flags"])
        r["bucket"] = bucket(r, os.getcwd())
        print(json.dumps(r, indent=1, default=list))
        return
    con = sqlite3.connect("file:%s?mode=ro" % DB_PATH, uri=True)
    con.execute("PRAGMA query_only=ON")
    con.text_factory = lambda b: b.decode("utf-8", "replace")
    rows = con.execute(
        "select id, cmdline, reasons_json, status, coalesce(cwd,''), coalesce(org,''), deferred_at, defer_at, coalesce(decided_by,'') from security_gate_requests"
    ).fetchall()
    gate = []
    for rid, cmd, rj, status, cwd, org, deferred_at, defer_at, decided_by in rows:
        try:
            reasons = json.loads(rj or "[]")
        except ValueError:
            reasons = [rj or ""]
        r = route(cmd or "", cwd)
        b = bucket(r, cwd)
        st = status + ("+deferred" if deferred_at else "")
        gate.append(dict(cmd=cmd or "", cat=r["cat"], fam=family(r), env=r["env"], flags=r["flags"], bucket=b, status=st,
                         reasons=reasons, rclasses=reason_classes(reasons, cmd or ""), org=org, decided_by=decided_by,
                         writes=r["writes"], cwd=cwd))
    N = len(gate)
    report = {"gate_total": N}
    cat_c = Counter(g["cat"] for g in gate)
    report["gate_by_category"] = [(c, cat_c[c], pct(cat_c[c], N)) for c in CATEGORIES if cat_c[c]]
    stat_by_cat = defaultdict(Counter)
    for g in gate:
        stat_by_cat[g["cat"]][g["status"]] += 1
    report["gate_status_by_category"] = {c: dict(v) for c, v in stat_by_cat.items()}
    report["gate_status_total"] = dict(Counter(g["status"] for g in gate))
    env_by_cat = defaultdict(Counter)
    for g in gate:
        env_by_cat[g["cat"]][g["env"]] += 1
    report["gate_env_by_category"] = {c: dict(v) for c, v in env_by_cat.items()}
    bc = Counter(g["bucket"] for g in gate)
    report["gate_by_bucket"] = {k: (bc[k], pct(bc[k], N)) for k in "ABCD"}
    fam_c = Counter(g["fam"] for g in gate)
    report["gate_by_family"] = [(f, n, pct(n, N)) for f, n in fam_c.most_common()]
    bstat = defaultdict(Counter)
    for g in gate:
        bstat[g["bucket"]][g["status"]] += 1
    report["gate_bucket_status"] = {k: dict(v) for k, v in bstat.items()}
    cb = defaultdict(Counter)
    for g in gate:
        cb[g["bucket"]][g["cat"]] += 1
    report["gate_bucket_categories"] = {k: dict(v) for k, v in cb.items()}
    # flags
    fc = Counter()
    for g in gate:
        for f in g["flags"]:
            if not f.startswith("unknown_cmd"):
                fc[f] += 1
    report["gate_flags"] = fc.most_common(30)
    report["gate_unknown_cmds"] = Counter(f for g in gate for f in g["flags"] if f.startswith("unknown_cmd")).most_common(15)
    # false positives by reason class: held today, router bucket A/B
    rc_tot = Counter()
    rc_fp = Counter()
    rc_ex = defaultdict(list)
    for g in gate:
        for rc in g["rclasses"]:
            rc_tot[rc] += 1
            if g["bucket"] in ("A", "B"):
                rc_fp[rc] += 1
                if len(rc_ex[rc]) < 3 and mask(g["cmd"], 150) not in [e[0] for e in rc_ex[rc]]:
                    rc_ex[rc].append((mask(g["cmd"], 150), g["cat"], g["env"], g["status"]))
    report["fp_by_reason"] = sorted(((rc, rc_tot[rc], rc_fp[rc], pct(rc_fp[rc], rc_tot[rc])) for rc in rc_tot), key=lambda x: -x[2])
    report["fp_examples"] = {k: v for k, v in rc_ex.items()}
    # specific FP probes requested by the brief
    probes = {
        "heredoc_edit_held": sum(1 for g in gate if "heredoc" in g["flags"] and g["cat"] == "file_edit_script"),
        "path_mention_not_exec": sum(1 for g in gate if "staypoint_mention" in g["flags"] and g["cat"] != "staypoint_self" and any("StayPoint or its guards" in r or "sensitive path" in r for r in g["reasons"])),
        "prod_rule_but_no_prod_write": sum(1 for g in gate if any("may write to prod" in r for r in g["reasons"]) and "prod_write_segment" not in g["flags"]),
        "prod_rule_sql_read": sum(1 for g in gate if any("may write to prod" in r for r in g["reasons"]) and g["cat"] == "sql_read"),
        "secret_fetch_substitution": sum(1 for g in gate if "secret_fetch" in g["flags"]),
        "nested_rule_not_agent": sum(1 for g in gate if any("nested agent" in r for r in g["reasons"]) and "nested_agent" not in g["flags"]),
    }
    report["fp_probes"] = probes
    # board-decided denials that the router would auto-allow (signal of legit denials vs noise)
    report["denied_but_A_or_B"] = sum(1 for g in gate if g["status"].startswith("denied") and g["bucket"] in ("A", "B"))
    report["approved_C"] = sum(1 for g in gate if g["status"].startswith("approved") and g["bucket"] == "C")
    # bucket C/D samples
    report["sample_C"] = [(mask(g["cmd"], 130), g["cat"], g["env"], g["status"]) for g in gate if g["bucket"] == "C"][:12]
    report["sample_D"] = [(mask(g["cmd"], 130), g["cat"], sorted(f for f in g["flags"] if "unknown" in f or "opaque" in f or "pipe" in f or "nested" in f), g["status"]) for g in gate if g["bucket"] == "D"][:15]

    # ---------------- run_steps
    rs = con.execute(
        "select coalesce(command,''), coalesce(title,''), coalesce(body,'') from run_steps where kind='run'"
    ).fetchall()
    cmds = Counter()
    no_text = Counter()
    for command, title, body in rs:
        if command.strip():
            cmds[command] += 1
        elif body and " " in title and not title.startswith(("mcp", "Run command", "Git preflight")) and not re.match(r"^[A-Z][A-Za-z]+$", title):
            cmds[title] += 1
        else:
            no_text[title if re.match(r"^[A-Za-z_]+$", title) or title.startswith("mcp") else "other"] += 1
    total = sum(cmds.values())
    run_cat = Counter()
    run_cat_distinct = Counter()
    run_bucket = Counter()
    run_fam = Counter()
    run_env = Counter()
    run_unknown = Counter()
    for c, k in cmds.items():
        r = route(c, "")
        run_cat[r["cat"]] += k
        run_cat_distinct[r["cat"]] += 1
        run_bucket[bucket(r, "")] += k
        run_fam[family(r)] += k
        if r["env"] == "prod":
            run_env["prod:" + r["cat"]] += k
        for f in r["flags"]:
            if f.startswith("unknown_cmd"):
                run_unknown[f] += k
    report["run_total_rows"] = len(rs)
    report["run_bash_total"] = total
    report["run_bash_distinct"] = len(cmds)
    report["run_no_text_by_title"] = no_text.most_common(12)
    report["run_by_category"] = [(c, run_cat[c], pct(run_cat[c], total), run_cat_distinct[c]) for c in CATEGORIES if run_cat[c]]
    report["run_by_bucket"] = {k: (run_bucket[k], pct(run_bucket[k], total)) for k in "ABCD"}
    report["run_by_family"] = [(f, n, pct(n, total)) for f, n in run_fam.most_common()]
    report["run_prod"] = dict(run_env)
    report["run_unknown_cmds"] = run_unknown.most_common(15)
    con.close()
    print(json.dumps(report, indent=1, default=list))


if __name__ == "__main__":
    main()
