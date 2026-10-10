# Opening task pages: `staypoint task open`

`staypoint task open <id|ref|name>...` (alias `staypoint open`) opens task pages of
the web UI in the Board's browser, all in one call. It replaces the
`~/.local/bin/sp-open` stopgap.

Each argument must name exactly one task, or nothing opens:

- a task id: `task-1234abcd`
- a reference recorded on the task: `STA-775` (the Paperclip label an imported task carries)
- the exact task name, case-insensitive

URLs are always `http://localhost:41421/tasks/<org>/<project>/<id>`, never
`127.0.0.1`. `--print` prints them without opening anything.

## Browser and profile

Set in `~/.staypoint/config.toml`:

```toml
[browser]
app = "Microsoft Edge"
profile_directory = "Profile 4"   # or: profile = "Main"
```

- No `app`: the macOS default browser via `open`.
- Edge or Chrome with a profile: the app binary runs with
  `--profile-directory=<dir> <urls...>`, which hands the URLs to the running
  instance's window for that profile. `profile = "Main"` is resolved to its
  folder through the browser's `Local State` file (`profile.info_cache`).
- Any other app, or no profile: `open -a <app> <urls...>`.

No shell is involved: the URLs are built from the task's stored id and passed
as separate arguments.

## Agents: `staypoint_task_open`

The StayPoint MCP server exposes the same thing as `staypoint_task_open
{task_ids: [...]}`. It opens the pages on the Board's Mac and returns the URLs.
Effect: read (no data change). Agents should call it whenever they need the
Board to act: Run Now, approve a Ship Review or interaction card.

Limits, so a looping agent cannot spam tabs:

- at most 10 tasks per call;
- at most 3 calls that open tabs per MCP session (one agent run); refused calls
  do not count;
- every id must exist, or nothing opens;
- only `localhost:41421` task pages are ever handed to the browser.
