# `loope status-line`: automate Claude usage-capture statusline setup

Issue: #78

## Problem

`docs/telemetry.md#capturing-claude-usage-optional` documents how to wire
`loope claude-usage-hook` into `~/.claude/settings.json` so the fleet
telemetry dashboard can show a worker's Claude Code 5-hour/7-day usage. Today
this is entirely manual: the user hand-edits JSON, constructing a
`tee >(...)` shell wrapper themselves and being careful not to clobber
whatever `statusLine` command they already have configured. This is fiddly
and error-prone, and the capability isn't discoverable — it's not mentioned
anywhere `loope --help` shows.

The issue asks for a `loope status-line` command that performs this wiring
automatically, needs a config file (to find the right Claude profile
directory), and is listed in `--help`.

## Decisions (from the issue author, via clarifying rounds)

1. The Claude profile directory is `claudeConfigDir` from `loope.json` if
   set, else `~/.claude` — the same field and default the daemon already
   uses for `CLAUDE_CONFIG_DIR` (`worker/config.go`).
2. The command takes a required `--config` flag pointing at that same
   `loope.json` (no new config file format).
3. If a `statusLine` command already exists, `status-line` auto-wraps it
   (preserves the user's existing visible statusline) rather than
   overwriting or refusing.
4. This is a one-time, idempotent setup: running it again when already
   configured is a no-op.
5. It supports undo/removal via a flag.

## Design

### Command surface

`loope status-line --config <FILE> [--remove]`

Dispatched through the existing `dispatchSubcommand` switch in
`worker/main.go` (alongside `claude-usage-hook`), so it takes over the whole
process before the daemon's own flag set is parsed. `--config` is required;
missing it prints usage to stderr and exits 2, matching the pattern parse
errors already use elsewhere in `main.go`.

### Resolving paths

- Load `--config` with the existing `LoadConfig` (`worker/config.go`) to get
  `ClaudeConfigDir` (already `~`-expanded by `LoadConfig`). If empty, default
  to `~/.claude`.
- Settings file path: `<that dir>/settings.json`.
- The command string this tool writes must reference `loope` by an absolute
  path (not bare `loope`) so it keeps working regardless of the shell's
  `PATH` when Claude Code invokes it — resolved via `os.Executable()`
  (and `filepath.EvalSymlinks` to dereference any `PATH`-found symlink, since
  `os.Executable()`'s doc explicitly does not guarantee that).

### Reading and writing settings.json

`settings.json` is parsed into a generic `map[string]json.RawMessage` (not a
fixed struct), so keys this tool doesn't know about (and any of Claude
Code's own settings) pass through untouched. Only the top-level `statusLine`
key is read/replaced. `statusLine` itself, when present, is expected to be
an object with a `type` and `command` field — this tool only ever writes
`{"type": "command", "command": "..."}`, matching the doc's existing
example.

If the settings file doesn't exist yet, treat it as `{}` (no existing keys)
and create the file (and its parent directory) on write.

Every time this tool is about to modify `settings.json`, it first writes the
current file's bytes to `settings.json.bak` (best-effort; a failure to back
up is logged to stderr but does not block the write) — cheap insurance
before mutating a file Claude Code itself depends on. No backup is written
on `--remove` when nothing needs to change (already-clean state).

### Recognizing "our" command

Two exact string templates, built from the resolved absolute `loope` path
(call it `$LOOPE`):

- **Wrapped** (an original command existed): <br>
  `sh -c 'tee >($LOOPE claude-usage-hook) | <original>'`
- **Bare** (no original command existed): <br>
  `sh -c 'tee >($LOOPE claude-usage-hook) >/dev/null'`

These are the only two shapes this tool ever writes, and the only two shapes
it recognizes as "ours" when deciding whether install is already done or
what remove should restore. `<original>` is captured as the literal
remainder of the string after the fixed `sh -c 'tee >($LOOPE claude-usage-hook) | `
prefix, up to the closing `'`.

### Install (default, no `--remove`)

1. Load config, resolve settings path and `$LOOPE`.
2. Read `settings.json` (or treat as `{}`).
3. Inspect `statusLine.command`:
   - Matches the **wrapped** or **bare** template already → print
     `status-line: already configured (<settings path>)` and exit 0. No
     write, no backup.
   - Some other non-empty command → back up, rewrite to the **wrapped**
     template embedding that command as `<original>`, write file, print
     `status-line: wrapped existing statusLine command in <settings path>`.
   - `statusLine` absent or empty → back up, write the **bare** template,
     write file, print `status-line: configured in <settings path> (no
     previous statusLine was set, so your status line will show no visible
     output — set your own command later to see one alongside usage
     capture)`.
4. Exit 0 on success. Any read/write/marshal error prints to stderr and
   exits 1.

### Remove (`--remove`)

1. Load config, resolve settings path and `$LOOPE`.
2. Read `settings.json`. If it doesn't exist, print
   `status-line: nothing to remove (<settings path> does not exist)` and
   exit 0.
3. Inspect `statusLine.command`:
   - Matches **wrapped** → back up, set `statusLine.command` back to the
     captured `<original>`, write file, print `status-line: restored your
     original statusLine command in <settings path>`.
   - Matches **bare** → back up, delete the `statusLine` key entirely,
     write file, print `status-line: removed from <settings path>`.
   - `statusLine` absent already → print `status-line: already removed
     (<settings path>)` and exit 0. No write.
   - Present but matches neither template (user hand-edited it since) →
     print `status-line: statusLine command in <settings path> was not set
     by this tool (or was modified since) — edit it manually` to stderr and
     exit 1. No write.
4. Exit 0 on success (or the already-removed no-op above).

### Help / discoverability

`usage()` (`worker/main.go`) gains a `Subcommands:` section beneath the
existing `Flags:` section, listing both `status-line` and the pre-existing
undocumented `claude-usage-hook`:

```
Subcommands:
  status-line --config <FILE> [--remove]
        wire (or unwire) Claude Code's statusLine to capture usage for the
        fleet dashboard; see docs/telemetry.md
  claude-usage-hook
        internal: reads statusLine JSON from stdin, writes the usage
        snapshot loope reads back (wired automatically by status-line)
```

This text is static (not driven by `flag.FlagSet`, since subcommands bypass
that flag set entirely) but lives in the same function so `--help` and bare
invocation both show it, matching today's behavior for the daemon's own
flags.

### Documentation

`docs/telemetry.md`'s "Capturing Claude usage (optional)" section is
rewritten to lead with:

```bash
loope status-line --config /path/to/loope.json
```

followed by one sentence on what it does (wraps or sets your
`~/.claude/settings.json` `statusLine`), and keeps the existing manual
`tee >(...)` example directly below as "what this does under the hood /
how to do it by hand", since that's still useful for users who want to
understand or hand-tune the result.

## Files touched

- `worker/main.go` — `dispatchSubcommand` gains a `status-line` case;
  `usage()` gains the `Subcommands:` block.
- New file `worker/status_line_cmd.go` — flag parsing for the subcommand,
  path resolution, JSON read/modify/write, and the install/remove logic
  described above.
- New file `worker/status_line_cmd_test.go` — unit tests for: fresh install
  (no prior `statusLine`), wrapping an existing command, idempotent re-run,
  remove after wrap (restores original), remove after bare install (deletes
  key), remove when already clean, remove when hand-edited (errors), and
  `claudeConfigDir` default vs. override.
- `docs/telemetry.md` — updated "Capturing Claude usage" section.

## Testing

Unit tests drive `worker/status_line_cmd.go`'s core logic (a function
taking/returning the settings bytes, or operating against a `t.TempDir()`
fixture) directly rather than through `os.Args`/process dispatch, following
the existing pattern in `worker/telemetry_usage_hook.go` /
`worker/main_test.go` (`dispatchSubcommand` is already unit-tested there for
`claude-usage-hook`; the same style extends to `status-line`). No new
integration/e2e test is needed — this is a self-contained file-editing
command with no network or daemon interaction.
