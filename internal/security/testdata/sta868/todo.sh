#!/bin/bash
D=/tmp/sta-cleanup
ls -d "$HOME"/Documents/dev/worktrees/*/ | sed 's#/$##' | sort > $D/all.txt
cut -f1 $D/wt.tsv | grep '^/' | sort -u > $D/done.txt
comm -23 $D/all.txt $D/done.txt > $D/todo.txt
echo "all=$(wc -l < $D/all.txt) done=$(wc -l < $D/done.txt) todo=$(wc -l < $D/todo.txt)"
echo "agent-mesh .worktrees: $(ls "$HOME"/Documents/dev/agent-mesh/.worktrees | wc -l)"
