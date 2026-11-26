# Harness adapters

An adapter turns the hook payloads of one AI harness into SeaTime `event.Event`s
(`pkg/event`). Each adapter lives in `internal/adapters/<harness>/`. All of them
are registered in `internal/adapters/registry.go`.

Common behavior:

- `seatime setup` calls `Detect()` then `Install(seatimeBin)`; `seatime uninstall` calls `Uninstall()`.
- Install is idempotent. It edits the JSON as structured data: it keeps unknown
  keys, but it normalizes key order. Before its first edit of a file it
  keeps one copy of the original, `<file>.seatime-bak`, or writes `<file>.seatime-absent` when there was no file. It marks its own entries by the shape of their
  command (`<path>/seatime[.exe] hook <name> <event>`). Uninstall removes exactly
  those entries. It also removes any containers that it left empty.
- The home directory comes from `SEATIME_FAKE_HOME` if it is set (for tests and
  dry runs). Otherwise it is the real home. While `SEATIME_FAKE_HOME` is set,
  adapters never touch the real config.
- `HandleHook` never panics. It makes no network calls and runs no git. It prints
  nothing unless the harness's protocol needs a response.
- `SEATIME_IGNORE=1` in the environment makes every hook a no-op.
- The hook command is `"<seatime>" hook <name> <Event>`. The binary path is put in
  double quotes when it contains spaces or special characters.

## Claude Code (`claude-code`) - verified

Docs: https://code.claude.com/docs/en/hooks. Config: `~/.claude/settings.json`.
Hooks installed: SessionStart, UserPromptSubmit, PostToolUse (all tools),
PostToolUseFailure (Bash), Stop, SessionEnd. Timeout 10 s.

Captured: session start and end, with the source or reason in `Extra`; the
prompt and agent_start; and agent_stop for the main agent only (a subagent's
Stop is ignored). Tool events for Write/Edit/MultiEdit/NotebookEdit have
Write=true and Files. Bash events carry the command and the exit code. The exit
code is 0 on PostToolUse; on PostToolUseFailure it is parsed from "Exit code N".
Files listed in `tool_response.bashEditDiff.changedFiles` (a beta field) are
captured too, and so are `run` events from the shell classifier.

Model (docs checked 2026-11-24): only SessionStart can carry `model`, and it is left out
after `/clear` and in a recovered session. No other event names the model. The session
log does: every assistant entry has `message.model`. So a tool call and a stop take the
model of the log's last assistant entry (`ModelInLog`; subagent entries and `<synthetic>`
ones are skipped). That is the model that made the call, also after `/model` or a
fallback. A prompt takes the session's last named model, which the client carries forward.

Who sent a prompt (checked on 2.1.332): Claude Code also runs UserPromptSubmit for messages that it makes itself,
such as a background task or a subagent that reports back, a message from another session or from a channel, and a
scheduled task. The hook's payload does not say who sent the prompt. The session log does: the line of the prompt
has `"origin":{"kind":"human"}` for a prompt that a person sent, and another kind (`task-notification`, `channel`,
`peer`, `coordinator`, `auto-continuation`, ...) for the rest. Claude Code writes that line only after the hook has
returned, so the hook cannot read it. The hook's `prompt_id` is the line's `promptId`. So SeaTime keeps the prompt
back, with what the hook read at that moment, and records it once the log says who sent it. That happens at the
session's next hook, or within a few seconds in the background service (`internal/client/emit/hold.go`). A prompt that a person
sent is recorded with the time at which it was sent. A message from the tool is not recorded as a prompt; only the
agent run that it started is. If the log does not say who sent a prompt (an older Claude Code), the prompt is
recorded as before. The one exception is a background task's report, which is known by its `<task-notification>`
text.

Headless `claude -p` runs are left out. The docs define no payload field that
marks them. Hooks inherit the environment of the claude process, so we drop the event
when `CLAUDE_CODE_ENTRYPOINT` starts with `sdk` or `CLAUDE_CODE_SESSION_ATTENDED`
is `0` or `false`. We saw these values on 2.1.326; they are not documented.
Interactive sessions show `cli` and `1`. Events with unknown values are kept. On
Windows, when the path contains spaces, the hook is
`& "C:\...\seatime.exe" hook ...` with `"shell": "powershell"`.

Gaps: there is no timing for each tool call. PreToolUse and SubagentStop are not
recorded. The headless check could break if Anthropic renames those env vars.

## OpenAI Codex CLI (`codex`) - config verified, payload details partly unverified

Docs: https://learn.chatgpt.com/docs/hooks. Config: `$CODEX_HOME/hooks.json`
(default `~/.codex/hooks.json`). We use hooks.json, not the TOML `[hooks]` table,
so that `config.toml` is never rewritten. Hooks: SessionStart, UserPromptSubmit,
PostToolUse, Stop, SessionEnd. The payload includes the model on every event.

Captured: sessions; prompt, agent_start and agent_stop; tool events; run events;
the shell command and exit code (best effort); and the file paths of
`apply_patch`, parsed from the patch text.

Gaps: Codex runs a non-managed hook only after the user reviews it once in
`/hooks`, so `seatime setup` should tell the user. We could not verify the exact
tool names or the shape of `tool_response`, so the parsing is defensive. We do
not use the legacy `notify` program, and we do not tail the JSONL files in
`~/.codex/sessions` (there is no Watcher).

## Cursor (`cursor`) - verified

Docs: https://cursor.com/docs/agent/hooks. Config: `~/.cursor/hooks.json`
(`{"version":1,"hooks":{...}}`). Hooks: sessionStart, beforeSubmitPrompt,
afterFileEdit, afterShellExecution, stop, sessionEnd.

Stdout: `beforeSubmitPrompt` always prints `{"continue":true}`, even after an
internal error. `stop` prints `{}`. The other hooks print nothing. We never use
`failClosed`.

Captured: prompt and agent_start; file edits (Write, the file, and a line count
from `edits[].new_string`); shell commands and run events; agent_stop, with the
status in Extra; and session start and end. The session id is
`conversation_id`, the cwd is `workspace_roots[0]`, and the model comes from the
payload.

Gaps: `afterShellExecution` has no exit code. Tab completions are not recorded.

## Gemini CLI (`gemini-cli`) - verified (events/protocol); tool names unverified

Docs: https://geminicli.com/docs/hooks/ , https://geminicli.com/docs/hooks/reference/.
Config: `~/.gemini/settings.json`. Hooks: SessionStart, BeforeAgent, AfterTool,
AfterAgent, SessionEnd, with a timeout of 10000 ms. We always print `{}` to
stdout, because Gemini accepts only JSON there.

Captured: prompt and agent_start (from BeforeAgent), agent_stop (from
AfterAgent), tool events, run events, and the payload's `timestamp`.
`write_file` and `replace` count as writes. For `run_shell_command`, the exit
code is parsed from "Exit Code: N" in `llmContent`.

Gaps: the payload has no model name. The tool names, and where the exit code is
found, are best effort.

## opencode (`opencode`) - best effort, NOT verified against a live install

Docs: https://opencode.ai/docs/plugins/ , https://opencode.ai/docs/build/plugins/server/.
Install writes `~/.config/opencode/plugins/seatime.js` (or writes it into
`plugin/` if only that folder exists). The file is made from the embedded
`plugin.js` (go:embed), with the path of the SeaTime binary put in as a JSON
string. It starts with `// seatime-managed:`. Uninstall deletes only files with
that marker, and install refuses to overwrite a `seatime.js` that it did not
write.

opencode's docs now describe a V2 plugin API (`Plugin.define`, `ctx.tool.hook`).
`plugin.js` uses the V1 hook keys (`event`, `chat.message`,
`tool.execute.before/after`). It exports both a default `{server}` and a named
function, and a guard stops it from loading twice. Check it again on a real
opencode, and switch to V2 hooks if V1 is gone.

The plugin calls SeaTime with `seatime hook opencode <session_start|session_end|prompt|agent_stop|tool>`
and JSON `{ts, session_id, cwd, model, text, tool, args, metadata}`.

## GitHub Copilot (`copilot`): CLI and VS Code agent mode - hooks, partly unverified

Docs: https://docs.github.com/en/copilot/reference/hooks-configuration,
https://docs.github.com/en/copilot/reference/hooks-reference,
https://code.visualstudio.com/docs/agent-customization/hooks (all read 2026-11-19).
Copilot CLI and the "Copilot harness" in VS Code read the same folder of user hooks, so one adapter covers
both. VS Code's older "Local" hooks harness (`chat.useHooks`, PascalCase events) is deprecated and due to be
removed in Feb 2027. SeaTime does not support it.

Config: SeaTime has its own file, `~/.copilot/hooks/seatime.json` (in `$COPILOT_HOME/hooks/` when that is
set). So SeaTime edits no other config, and uninstall just deletes the file. Its content looks like this:
`{"version":1,"hooks":{"userPromptSubmitted":[{"type":"command","command":"seatime hook copilot userPromptSubmitted","timeoutSec":10}]}}`.
Events installed: sessionStart, userPromptSubmitted, postToolUse, agentStop, sessionEnd. On Windows, when
the path needs quotes, an extra `powershell` key holds `& "<seatime.exe>" hook ...`.

Captured: session start and end, prompt and agent_start, agent_stop (main agent only), tool events, shell
commands and run events. A tool is a write tool when its name contains edit, create, write, patch, replace
or insert; its file comes from `toolArgs.path`. The exit code of a shell command comes from
`toolResult.resultType` or from "exit code N" in the text. The payload is parsed in camelCase (`sessionId`,
`toolName`, `toolArgs` as an object or a JSON string, `timestamp` in ms since the epoch). It is also parsed
in the PascalCase/snake_case form (`session_id`, `tool_name`, `tool_input`, ISO timestamp).

Unverified: whether the CLI sends the payload on stdin (its docs only say "JSON payload"; the VS Code docs
say stdin), the exact tool names, and the shape of `toolResult`. Detect: `~/.copilot` exists, or the
`github.copilot-chat-*` VS Code extension is installed. If hooks break, a fallback is to tail
`~/.copilot/session-state/<id>/events.jsonl`. Each event there has an RFC 3339 `timestamp`, and
`user.message` has the prompt in `data.content`. This fallback is not implemented.

## Windsurf (`windsurf`) - hooks, config shape from secondary sources

Windsurf's hooks page now redirects (docs.windsurf.com -> docs.devin.ai/desktop/cascade/hooks) and could not
be opened. The shape below comes from https://clients.dev/clients/windsurf?tab=hooks,
https://pkg.go.dev/github.com/Checkmarx/ast-cx-hooks/windsurf and
https://docs.rs/crate/lean-ctx/3.7.5/source/src/hooks/agents/windsurf.rs.
Config: `~/.codeium/windsurf/hooks.json`, in a flat shape: `{"hooks":{event:[{"command":...,"show_output":false}]}}`.
There is no `type` key. That is why `common.HookSpec.Extra` treats a nil value as "omit this key".
Events: pre_user_prompt (`tool_info.user_prompt`) -> prompt + agent_start, post_cascade_response -> agent_stop,
post_write_code (`tool_info.file_path`) -> write tool event, post_run_command (`tool_info.command_line`,
`tool_info.cwd`) -> tool + run. Fields in every payload: `trajectory_id` (session), `execution_id`, `timestamp`,
`model_name`. Exit code 0 means proceed. We print nothing.
Unverified: the Windows path, what stdout must hold, and the hook's working directory. That directory is
used as a fallback for the cwd, unless it is `$HOME`. Detect: `~/.codeium/windsurf`. A newer
`post_cascade_response_with_transcript` event writes `~/.windsurf/transcripts/<trajectory_id>.jsonl`. We do
not need it.

## Google Antigravity (`antigravity`) - hooks, verified against the official docs

Docs: https://antigravity.google/docs/hooks. Config: `~/.gemini/config/hooks.json`. Each top-level key is a
hook *name*. SeaTime owns the key `seatime`. It leaves other keys alone, and it never overwrites a `seatime`
group that it did not write. Example:
`{"seatime":{"PreInvocation":[{"type":"command","command":"seatime hook antigravity PreInvocation","timeout":10}],...}}`.
Events installed: SessionStart, PreInvocation, Stop. Payload (camelCase): `conversationId` (session),
`workspacePaths[0]` (cwd), `modelName`, `lastUserInput`, `invocationNum`, `fullyIdle`. stdout must be JSON
only, so we always print `{}`.
Mapping: PreInvocation -> prompt (`lastUserInput`) + agent_start; Stop -> agent_stop unless `fullyIdle:false`.
The docs do not say whether PreInvocation fires once per user message or once per model call. So a prompt is
recorded only when `invocationNum` is 1 or missing, or when `lastUserInput` differs from the last one stored
for that conversation (in `<data dir>/adapters/antigravity/last_input.json`). Detect:
`~/.gemini/antigravity{,-ide,-cli}`. Transcripts (`<app dir>/brain/<id>/.system_generated/logs/transcript.jsonl`)
are not tailed, because we could not confirm that each record has a timestamp.

## Kilo Code (`kilo` plugin, `kilo-code` legacy storage)

Kilo has shipped two versions. The rebuilt extension and CLI are based on opencode and became generally
available in April 2026. They load TypeScript and JavaScript plugins from `~/.config/kilo/plugin/`, with the
same hook keys that opencode uses (https://kilo.ai/docs/automate/extending/plugins). So `kilo` reuses
opencode's embedded `plugin.js` (`opencode.NewKilo()`). It is installed as `~/.config/kilo/plugin/seatime.js`
and calls `seatime hook kilo <event>`. As with opencode, the plugin payloads are not verified on a live Kilo.
Detect: `~/.config/kilo` or `~/.local/share/kilo`. Kilo also keeps its sessions in SQLite at
`~/.local/share/kilo/kilo.db` (tables session/message/part,
https://kilo.ai/docs/code-with-ai/agents/session-history). We do not read it, because the plugin is enough.
The legacy v5 line stores its tasks in Cline's format. The Cline family adapter below covers it (`kilo-code`).

## Cline, Roo Code and legacy Kilo Code (`cline`, `roo-code`, `kilo-code`) - log watcher, fixtures only

The VS Code extensions have no hooks that we can use. Cline's classic hooks are executable files in
`~/Documents/Cline/Hooks/`, named after events. Two sources disagree on that path, the hooks run only on macOS
and Linux, and the nesting of the payload is unclear. In 2026 Cline's SDK and CLI replaced them with plugins,
and those plugins do not apply to the extension
(https://docs.cline.bot/getting-started/config.md, https://docs.cline.bot/customization/plugins.md). Roo Code
shipped no hooks, and the extension shut down on 2026-05-15 (existing installs keep working). So SeaTime tails
the task storage that these extensions share. The layout comes from
https://raw.githubusercontent.com/getagentseal/codeburn/main/docs/providers/cline.md and
`src/providers/vscode-cline-parser.ts`: `<VS Code|Insiders|VSCodium|Cursor|Windsurf User dir>/globalStorage/<extension id>/tasks/<taskId>/ui_messages.json`.
The file is a JSON array that is rewritten as a whole, and every message has a `ts` in ms. Extension ids:
`saoudrizwan.claude-dev`, `rooveterinaryinc.roo-cline`, `kilocode.kilo-code`.
Mapping: `say` task/user_feedback/first text -> prompt + agent_start; `say:"tool"` (editedExistingFile, newFileCreated,
appliedDiff, insertContent, searchAndReplace, fileDeleted) -> write tool event; `ask:"command"` -> tool + run (no exit
code); `ask` completion_result/followup/plan_mode_respond/act_mode_respond -> agent_stop. The cwd comes from the
"Current Working Directory (...)" line in `api_conversation_history.json`. This is best effort, and the cwd can be empty.
The watcher polls every 5 s and checks each file's mtime. It keeps a message count for each task in
`<data dir>/adapters/<name>/offsets.json`. It never replays history: when it first sees a task, it skips the task
unless it started less than 2 min ago. It drops events older than 15 min.
Not covered: Cline CLI and SDK sessions (`~/.cline/data/sessions/<id>/<id>.messages.json`), which use a different format.

## Aider (`aider`) - log watcher

Aider has no hooks. `--notifications-command` runs only when the model has finished, and it passes no payload
(https://aider.chat/docs/usage/notifications.html). So SeaTime tails `.aider.chat.history.md` in each project.
The format comes from https://raw.githubusercontent.com/Aider-AI/aider/main/aider/io.py. User lines start with
`#### `, assistant text is plain Markdown, tool output starts with `> `, and headers read
`# aider chat started at <local time>`. The file has no timestamp for each message, so each event gets the time
of the poll (to within 5 s). A new `####` block is a prompt and agent_start. Assistant text followed by 4 s with
no new text is agent_stop, timed at the moment that the file last grew. `> Applied edit to <file>` is a write
tool event.
Finding the files: SeaTime looks up to 3 levels deep under `$HOME`, `~/code`, `~/projects`, `~/dev`, `~/src`,
`~/repos`, `~/work`, `~/Documents`, `~/Desktop` and `SEATIME_AIDER_DIRS`. It looks at no more than 6000
directories, and it scans again every 3 min. It does not find history files that were moved with
`--chat-history-file`. A new file is read from its current end. Detect: `aider` on PATH or `~/.aider.conf.yml`.
Not checked on a live aider: the `Applied edit` line, and exactly when aider flushes the file.

## Amp (`amp`) - thread file watcher

Amp can be extended only through a TypeScript plugin API that runs inside Amp (`amp.on("agent.start")`, ...;
https://ampcode.com/docs/customize/plugins). It has no hooks that take JSON on stdin, and no documented payload
schema. So SeaTime tails `~/.local/share/amp/threads/T-*.json` (`$AMP_DATA_DIR` overrides this). It never opens
the `secrets.json` file next to them. The layout comes from third-party parsers
(https://docs.rs/crate/lore-cli/latest/source/src/capture/watchers/amp.rs). `messages[]` has `role`, `content[]`
blocks (text / tool_use / tool_result) and `meta.sentAt` (ms). `env.initial.trees[0].uri` is the workspace.
Mapping: user message with text -> prompt + agent_start (time = sentAt); assistant `tool_use` edit_file / create_file /
undo_edit -> write tool event, Bash -> tool + run; a text-only assistant message -> agent_stop. Offsets and history
work as in the Cline family: offsets are saved, and history is never replayed. The Windows path is not verified. If
Amp's plugin API becomes stable, a better way is a 10-line plugin that calls `seatime emit` (see PLUGINS.md).

## Queued prompts (typed while the agent runs)

When you type a prompt while the agent is still working, the tool holds it and sends it when the run ends. So its
record looks like a reply that took 0 seconds. When the tool's own log says when the prompt was typed, the prompt
carries `queued_ms` (how long it waited). Facts about reply time then leave the prompt out. The rule still counts the
prompt at the time when it was sent.

- **Claude Code:** read. The session log (`transcript_path`) has a line `{"type":"queue-operation","operation":"enqueue",
  "timestamp":...,"sessionId":...,"content":...}` for each typed message. The hook finds the newest such line whose
  text matches the prompt (`claudecode.TypedAt`). Recent versions log every prompt this way, a few milliseconds
  before they send it. So a prompt counts as queued only if it waited a second or more, and was typed after the
  session's previous prompt.
- **Codex:** not read. A prompt typed during a run is either sent into the running turn ("steer") or held by
  `codex queue`. We have not found the typing time in its session files, and the adapter does not read them.
- **Cline family, Aider, Amp:** not read. Their logs keep one time for each message. We found no separate typing time.
- **opencode / Kilo plugin:** not read. The plugin gets the prompt when it is sent. We have not verified whether the
  server keeps the time when a prompt was queued.

## AI edits seen only through the editor plugin (`ai coding`)

The WakaTime proxy keeps the heartbeats that the plugin marks `ai coding`. It stores them as the agent's heartbeats, with the editor as the tool.
While the AI check is off, they count only in the 10 minutes after a hand edit or new prompt in the same project (`docs/RULE.md`, definition 3).
While the check is on, they do not count by themselves. Only a prompt that passes the check counts, for up to 10 minutes (15 when the person decides how part of the project should work). Hand edits count as Hackatime counts them (`docs/RULE.md`, "When the AI check is on").
Live test: not run as of 2026-11-21. The test is an agent that edits an open file, with the plugin pointed at the proxy and the agent's hook switched off. This server has no editor with a WakaTime plugin, and the test must not touch `~/.wakatime.cfg` here. It still has to be run on a machine that has an editor plugin.
Until it runs, we do not know whether a given plugin marks an agent's edit as `ai coding`. No other detector is used in its place.

## Not implemented, and why

- **Zed.** Zed has no hooks (https://zed.dev/docs/ai/agent-panel). Its native agent threads are in `threads.db` (SQLite,
  WAL; Linux `~/.local/share/zed/threads/threads.db`, macOS `~/Library/Application Support/Zed/threads/`, Windows
  `%LOCALAPPDATA%\Zed\threads\`). But the message bodies are zstd-compressed JSON. The Go standard library has no zstd,
  so reading them would add a dependency. And the only timestamp is the thread's `updated_at`, so the times of prompts
  and turns cannot be recovered. Zed's *external agents* (ACP: Claude Agent, Gemini CLI, Codex) run the real CLIs, which
  SeaTime already hooks. So most AI use in Zed is covered, and those agents need no plugin. For the native agent, wait
  for hooks, or ship a Zed extension that calls `seatime emit`.
- **Continue.** We found no hooks in the docs or the CLI reference (https://docs.continue.dev/cli/configuration).
  Continue always writes local dev data to `~/.continue/dev_data/<schema>/chatInteraction.jsonl`. Its fields are
  `prompt`, `completion`, `sessionId` and an ISO `timestamp`, which is written when the interaction *ends*. But `prompt`
  is the full prompt sent to the model, with the context that Continue adds. That would make word counts and novelty
  look too high. There is also no start time. `cn` CLI sessions (`~/.continue/sessions/<id>.json`) have no timestamps
  at all. A plugin could be a Continue "custom command" or MCP wrapper that calls `seatime emit`. Or SeaTime could tail
  `chatInteraction.jsonl` once it has a field with only the prompt. The project's status in late 2026 is also unclear.
- **Roo Code hooks** (draft PRs only) and **Cline's script hooks** are not used. See above.

## Writing a new adapter

1. Create `internal/adapters/<name>/` with a type that implements `common.Adapter`.
   It has the same methods as `adapters.Adapter`. Use `common.Sink` and `common.Home()`.
   The package cannot import package `adapters`, because that would make an import
   cycle. `registry.go` wraps it instead.
2. Reuse the shared helpers. Use `common.InstallHooks/UninstallHooks/HooksInstalled`
   for JSON hook configs (`MatcherGroups` or `Flat`). Use `common.Safe` in
   HandleHook. Use `runclass.Classify(cmd)` to emit `run` events, through
   `common.EmitRun`.
3. Set Harness, SessionID, CWD, Model and TS when they are present. Leave Project,
   Seq and the prompt features to the client. The client gives every record of a
   session the project that the session was started in (docs/RULE.md, definition 4),
   so always set SessionID and CWD. A record with no Model takes the last one that a
   record of its session named.
4. Add payload fixtures in `testdata/*.json`, a golden test that uses
   `common.Collector.Summary()`, and `common.HookRoundTrip` to test install and uninstall.
5. Add it to `registry.go` and to this document, with a link to the harness's docs.

Some harnesses are read by tailing their files. They implement `Watch(ctx, stateDir, sink)` and embed `common.WatchOnly`.
There is nothing to install: once `Detect()` is true, setup reports them as in place. Keep a `Poll(now, sink)` method so
that tests can run it with a fake clock. Save offsets with `common.Offsets`, and never replay history (`common.Stale`).
The daemon wraps the sinks of watchers. So `seatime uninstall --harness <name>` stops their events, even though there
are no hooks to remove.

## Plugin protocol

See [PLUGINS.md](../PLUGINS.md). A plugin sends events to `seatime emit` on stdin, or POSTs them to localhost with authentication.

## Auto-hooking and opt-outs

`seatime setup` hooks every harness that it detects. After that, the daemon runs detection again shortly after it starts,
and then every 10 minutes (`internal/client/setup/hooks.go`, `internal/client/daemon/autohook.go`). A harness installed
later gets hooks (`auto-hook: new harness X detected, hooks installed` in `seatime.log`). Hooks that a harness update
wiped out are installed again (`... repaired`). The daemon also repairs a harness that the canary finds silent. It
reports the results at each check-in, so the Settings page shows which tools are hooked and what is wrong.
`seatime doctor` does the same steps when you run it (`--no-fix` only reports).

Opt-outs, in `config.json` in the data dir:

- `"auto_hook": false` stops the daemon from changing any harness config. `seatime setup` and `seatime doctor` still change it when you run them.
- `seatime uninstall --harness codex` writes `"hook_opt_out": ["codex"]`. After that, neither setup nor the daemon hooks
  codex again. `seatime setup --harness codex` removes the opt-out and hooks it. (`seatime uninstall` without `--harness` clears all opt-outs.)
- `"hooked"` lists the harnesses that SeaTime has hooked, so that a missing hook is logged as a repair. `"service"` records how the daemon is run.

The adapter interface did not change. The daemon needs only `Detect`, `Installed` and `Install` (idempotent).
