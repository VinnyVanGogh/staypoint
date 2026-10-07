#!/bin/bash
# read-only worktree audit. args: worktree dirs. prints TSV.
# cols: path size_mb repo branch dirty ahead_upstream unpushed_any upstream remote_branch_exists merged_main last_commit_date
declare -A LSR
for d in "$@"; do
  d=${d%/}
  [ -e "$d/.git" ] || { printf '%s\t%s\tNOGIT\n' "$d" "$(du -sm "$d" 2>/dev/null|cut -f1)"; continue; }
  common=$(git -C "$d" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)
  repo=${common%/.git}; repo=${repo%%/.git/*}
  size=$(du -sm "$d" 2>/dev/null | cut -f1)
  br=$(git -C "$d" symbolic-ref --short -q HEAD || echo "DETACHED@$(git -C "$d" rev-parse --short HEAD)")
  dirty=$(git -C "$d" status --porcelain 2>/dev/null | wc -l | tr -d ' ')
  up=$(git -C "$d" rev-parse --abbrev-ref -q '@{u}' 2>/dev/null || echo -)
  ahead=-; [ "$up" != - ] && ahead=$(git -C "$d" rev-list --count '@{u}..HEAD' 2>/dev/null)
  unp=$(git -C "$d" rev-list --count HEAD --not --remotes 2>/dev/null)
  key="$repo"
  if [ -z "${LSR[$key]+x}" ]; then
    LSR[$key]=$(git -C "$d" ls-remote --heads origin 2>/dev/null | awk '{sub("refs/heads/","",$2); print $2}' | tr '\n' ' ')
    [ -z "${LSR[$key]}" ] && LSR[$key]="__LSFAIL__"
  fi
  rb=n/a
  if [[ "${LSR[$key]}" == "__LSFAIL__" ]]; then rb=lsfail
  elif [[ "$br" != DETACHED* ]]; then [[ " ${LSR[$key]} " == *" $br "* ]] && rb=yes || rb=no; fi
  mb=-; for m in origin/main origin/master; do git -C "$d" rev-parse -q --verify $m >/dev/null && { git -C "$d" merge-base --is-ancestor HEAD $m && mb=yes || mb=no; break; }; done
  lc=$(git -C "$d" log -1 --format=%cs 2>/dev/null)
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$d" "$size" "$repo" "$br" "$dirty" "$ahead" "$unp" "$up" "$rb" "$mb" "$lc"
done
