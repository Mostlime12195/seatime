# SeaTime plugin protocol

Any program can report to SeaTime: a shell prompt hook, an editor extension, a wrapper around a
CLI, or a script that reads a tool's log as it grows. You send **events** (see `pkg/event`). SeaTime
does the rest: it finds the project, adds the events to the hash chain, counts the time and syncs.

There are two ways to send events. Either way, a working integration takes under 30 lines.

## 1. `seatime emit` (stdin, no daemon needed)

```sh
echo '{"kind":"prompt","harness":"my-tool","session_id":"s1","text":"why does the parser panic?"}' | seatime emit
```

It reads one JSON event, a JSON array, or JSON Lines. Flags give defaults for missing fields:
`--harness`, `--session`, `--project`, `--cwd` (default: the current directory). The exit status is
non-zero when the input is not valid. Run it from inside the project, or pass `--cwd`. SeaTime then
counts the event for the project of the nearest git root. A folder is one project for good: it
stays the same project when its `origin` remote is added or changes.

Example: wrap any CLI so that each run counts as a prompt and an agent turn (bash, 8 lines):

```sh
#!/bin/sh
# usage: ai-wrap <tool> [args...]   e.g. ai-wrap sgpt "explain this stack trace"
sid="ai-wrap-$(date +%s)"
printf '{"kind":"prompt","session_id":"%s","text":%s}\n' "$sid" "$(printf '%s' "$*" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')" \
  | seatime emit --harness ai-wrap
echo '{"kind":"agent_start","session_id":"'"$sid"'"}' | seatime emit --harness ai-wrap
"$@"; rc=$?
echo '{"kind":"agent_stop","session_id":"'"$sid"'"}' | seatime emit --harness ai-wrap
exit $rc
```

## 2. Localhost HTTP (needs `seatime daemon`, which `seatime setup` keeps running)

Send the same payload as for stdin to `POST http://127.0.0.1:47823/seatime/emit`. Add the
**secret** that each install has:

```sh
curl -X POST http://127.0.0.1:47823/seatime/emit \
  -H "Authorization: Bearer $(seatime token)" \
  -d '[{"kind":"edit","files":["/abs/path/main.go"],"write":true}]'
```

The answer is `202 {"accepted":N}` on success, `400` with a message for a bad payload, `401` without
the secret, and `403` for a wrong Host or Origin. SeaTime makes the secret the first time that it is
needed. It is kept in `local_auth.json` in the SeaTime data directory, with mode 0600:
`~/.local/share/seatime`, `~/Library/Application Support/seatime` or `%LOCALAPPDATA%\seatime`.
`seatime token` prints it, and `seatime token rotate` replaces it. Your plugin should read it when it
starts, and not have it written into its code.

**Why a secret.** Any web page can make your browser call `127.0.0.1`. So the daemon refuses requests
whose `Host` is not a loopback name, which stops DNS rebinding. It refuses every request that has an
`Origin` header, as the requests that a browser sends do. And it requires the secret.
Scripts and CLIs send no `Origin`, so they only need the secret. The WakaTime-compatible endpoint does
not need the secret, so editor plugins can use it; they send no `Origin` either. It still refuses
anything from a browser or from a host name that was rebound.

Example: a Python plugin for a tool that has a "before send / after answer" callback (22 lines):

```python
import json, os, subprocess, time, urllib.request

URL = "http://127.0.0.1:47823/seatime/emit"
SECRET = subprocess.check_output(["seatime", "token"], text=True).strip()

def emit(*events):
    req = urllib.request.Request(URL, json.dumps(list(events)).encode(),
                                 {"Authorization": "Bearer " + SECRET})
    try: urllib.request.urlopen(req, timeout=2)
    except OSError: pass                      # never break the host tool because seatime is down

def on_user_prompt(session, text, cwd):       # call these from your tool's callbacks
    emit({"kind": "prompt", "harness": "my-tool", "session_id": session, "cwd": cwd, "text": text},
         {"kind": "agent_start", "harness": "my-tool", "session_id": session, "cwd": cwd})

def on_agent_done(session, cwd):
    emit({"kind": "agent_stop", "harness": "my-tool", "session_id": session, "cwd": cwd})

def on_file_written(session, cwd, path):
    emit({"kind": "tool", "harness": "my-tool", "session_id": session, "cwd": cwd,
          "tool": "write", "write": True, "files": [path]})
```

Node.js (global `fetch`, 12 lines):

```js
import { readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
const secret = execFileSync("seatime", ["token"]).toString().trim();
export const emit = (...events) =>
  fetch("http://127.0.0.1:47823/seatime/emit", {
    method: "POST", headers: { Authorization: `Bearer ${secret}` }, body: JSON.stringify(events),
  }).catch(() => {});
emit({ kind: "prompt", harness: "my-tool", session_id: "s1", text: "refactor the router" },
     { kind: "agent_start", harness: "my-tool", session_id: "s1" });
```

PowerShell (Windows, 6 lines):

```powershell
$secret = (seatime token)
$body = '[{"kind":"prompt","harness":"my-tool","session_id":"s1","text":"why is this slow?"}]'
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:47823/seatime/emit `
  -Headers @{ Authorization = "Bearer $secret" } -Body $body | Out-Null
```

## Event fields

Required: `kind`, one of `session_start session_end prompt agent_start agent_stop tool edit commit run focus`.
Everything else is optional. If you leave out `ts`, SeaTime uses the current time. SeaTime always sets
`seq` itself and ignores any value that you send. For a prompt, SeaTime also uses `text` to count the
words and to decide whether the prompt is new.

| field | for | notes |
|---|---|---|
| `ts` | all | RFC 3339; default now. Send the real time of the observation |
| `harness` | all | your tool's name (`editor:<name>` for editors); default `plugin`. Names starting with `web:` are refused: browser chats are not tracked |
| `session_id` | all | groups prompts and turns; strongly recommended for prompts |
| `cwd`, `files`, `project` | all | attribution inputs; `project` overrides attribution |
| `text` | prompt | the prompt. Stays on this machine; only a keyed hash of it is ever sent (and the text itself only for the AI check) |
| `tool`, `files`, `write`, `command`, `exit_code` | tool | `write:true` marks file modifications |
| `files`, `write`, `lines` | edit | human editor activity |
| `command`, `exit_code`, `run_kind` | run | `run_kind`: `test build dev exec` |

A good plugin sends `prompt` when the person sends a prompt. It sends `agent_start` and `agent_stop`
around each agent turn; `agent_stop` means that control went back to the person. It sends `tool` for the
agent's tool calls; writes are the best evidence of which project the work is for. It sends `run` for
test and build commands, and `edit` when the person types in an editor.

While the server's AI check is off, the agent's time counts only in the 10 minutes after a new prompt or
a hand edit. While the check is on, the agent's time does not count by itself. A prompt counts only if
the check passes it, for up to 10 minutes, or up to 15 when the person decides how part of the project
should work. Hand edits count as Hackatime counts them. Those two numbers are a server setting
([docs/RULE.md](docs/RULE.md#when-the-ai-check-is-on)). Either way, a missing `agent_stop` never adds
time past those minutes. Use `harness` names that are not the same as the built-in ones
(`seatime setup` lists them).

Time counts for the project that the tool works in, so send `cwd` or `project`
(`"project": "github.com/you/repo"`) with every event. A tool with no project directory, such as a chat
website, can't be tied to a project and is not tracked.

## Privacy and safety

* SeaTime removes secrets from `text` and `command` **before** it stores them: API keys, tokens, `.env`
  values, emails and the names of home directories. It stores paths relative to the project root. Don't
  count on this as your only protection.
* Nothing leaves the machine except totals for each minute, checkpoints of the chain and hashes of
  prompts ([pkg/api/WIRE.md](pkg/api/WIRE.md) lists every field).
* SeaTime adds each event to a hash chain as it writes it. So nobody can add or change events later
  without it showing. Don't make up events: the server checks them against the checkpoints that it got
  while the work happened, and it also runs statistical checks.

## WakaTime-compatible endpoint (editors)

`seatime setup --wakatime-proxy` sets `api_url = http://127.0.0.1:47823/api` in `~/.wakatime.cfg`.
`seatime setup --yes` does the same whenever `~/.wakatime.cfg` exists; `--no-wakatime` turns that off.
The original file is saved next to it (`.seatime-bak`), and `seatime uninstall` restores it byte for
byte. If you edited the cfg after setup, only the `api_url` line and the placeholder `api_key` line are
changed back. The daemon reads the address that it forwards to from `config.json` again each time that
file changes. If the daemon can't be started, `--yes` leaves the cfg as it was.

SeaTime never installs editor extensions. If you install the WakaTime plugin after SeaTime, the daemon
connects it within about 10 minutes. This works only if setup ran with `--yes` and without
`--no-wakatime` (`"wakatime_auto"` in `config.json`). Every WakaTime or Hackatime plugin then sends SeaTime
`edit` events. Every request is also forwarded **unchanged** to your previous `api_url` (for example
Hackatime), so it sees exactly what it saw before SeaTime. "Also send editor time to Hackatime" on the
Settings page turns this forwarding off and on for every computer of the account.
`seatime setup --also-hackatime=false` does the same on one computer, until the setting changes on the
website. A heartbeat with the category `ai coding` counts as the agent's, not yours. Browsing and
meetings are ignored. Nothing from the hooks of AI tools or from plugins is sent to Hackatime.

## Hooks

The adapter for each AI tool calls `seatime hook <adapter> <event>`, with the tool's payload on stdin.
The command always exits 0 and never holds up the AI tool for more than a couple of seconds. It writes problems to
`seatime.log` in the data directory. To write a Go adapter instead of a plugin, see `docs/ADAPTERS.md`.
