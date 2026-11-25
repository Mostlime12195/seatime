# What leaves your machine

Every field SeaTime can send to the server, with what it means. A test (`pkg/api/wire_test.go`) fails if a wire type gains a field that is not listed here, so this list cannot fall behind the code. `seatime inspect` prints the exact bytes of the next sync.

Never sent, by a sync or a segment: prompt text, folder paths, shell commands, window titles, keystrokes, screenshots, and no file path outside the project's git repo or absolute path. `inspect_test.go` and `segment_test.go` plant each of them in a log and fail if any appears in what would be sent.

A project's folder name is sent, as the project's name in a sync (`sync.projects[].name`): the name of the folder, never the path to it. A segment does not name the folder.

Prompt text is the one exception, and only while the server's prompt check is switched on (it is off unless the server turns it on; `seatime status` says when it is on). Then each prompt that is not an exact repeat is sent for the check, with the AI's reply before it, a short excerpt of the code change after it, and three facts about the project: its name, some of its file names and the start of its README. All of it is sent for the check, never kept: the server passes it to the checking model (OpenRouter, with providers that keep no data), stores only the yes/no answers, the prompt's hash and the model's name, and drops the rest. It never goes into a sync, a segment, the server's database or its logs. The tests above still fail if text shows up in a sync or a segment, and `promptcheck_test.go` in the server fails if any of it reaches the database or the log. A segment does name the files you edited inside the project's git repo, by their path in the repo (`src/app.go`), next to the editor heartbeats; your editor plugin already sends those paths, in full, to Hackatime.

## Every 60 to 120 seconds while you work (sync)

- `sync.buckets`: one entry per minute per project that had activity
- `sync.buckets[].ai_not_counted_seconds`: seconds of the AI agent's work in that minute that did not count, because they came more than 10 minutes after your last heartbeat
- `sync.buckets[].ai_seconds`: counted seconds in that minute that ended at the AI agent's heartbeat
- `sync.buckets[].coding_seconds`: counted seconds in that minute that ended at your own heartbeat (a hand edit or a new prompt)
- `sync.buckets[].commits`: how many commits
- `sync.buckets[].edits`: how many editor heartbeats
- `sync.buckets[].harnesses`: names of the AI tools or editors used in that minute, such as claude-code or editor:vscode
- `sync.buckets[].models`: that minute's counted AI seconds and prompts by AI model, for the records where the AI tool named its model
- `sync.buckets[].models[].model`: the model's name, as the AI tool reported it
- `sync.buckets[].models[].prompts`: how many of the minute's prompts that model answered (a count, never the text)
- `sync.buckets[].models[].seconds`: counted seconds in that minute with that model
- `sync.buckets[].project`: project key, which a folder keeps from the first time SeaTime sees it: the public GitHub/GitLab path if the repo was on a public forge then, otherwise local: plus a 12-character hash of the folder name
- `sync.buckets[].prompts`: how many prompts you sent (a count, never the text)
- `sync.buckets[].runs`: how many test, build or run commands
- `sync.buckets[].start`: the minute (UTC)
- `sync.checkpoint`: proof that your local log existed at this moment (two fields below)
- `sync.checkpoint.hash`: a SHA-256 fingerprint of your local log; reveals nothing about its contents
- `sync.checkpoint.seq`: how many records your local log holds
- `sync.checkpoint_sig`: signature over the checkpoint with this device's key
- `sync.client_version`: seatime's version number
- `sync.commits`: commits you made in tracked projects since the last sync
- `sync.commits[].del`: lines removed
- `sync.commits[].ins`: lines added
- `sync.commits[].project`: project key
- `sync.commits[].sha`: the commit id (lets the server match it on GitHub for public repos)
- `sync.commits[].ts`: commit time
- `sync.from`: start of the window this request describes in full (the server replaces what this device sent for it)
- `sync.device_id`: random id of this installation, made when it first signs in; not your name
- `sync.pending`: only while the prompt check is on: time of the last 24 hours that waits for the check (fields below), shown on your profile as "12 min waiting on check"
- `sync.pending.projects`: one entry per project with time waiting
- `sync.pending.projects[].project`: project key
- `sync.pending.projects[].seconds`: seconds waiting for the check
- `sync.projects`: one entry per project key that appears above
- `sync.projects[].key`: the same project key
- `sync.projects[].label`: what the project's folder goes by now, as a project key is written (the public GitHub/GitLab path, otherwise local: plus a 12-character hash). It is the key until a remote is added to the folder or its remote changes; the project stays the same one
- `sync.projects[].name`: the name you chose with `seatime name`, or a public repo's own name, or else the name of the project's folder (for a repo with a private remote, the repo's name). Only the name, never the path to the folder. Your profile shows it; you can give the project another name on the Settings page, or hide the project
- `sync.projects[].folder`: whether that name is the folder's name (yes or no). The server uses a folder's name only for a project that nobody has named
- `sync.projects[].remote`: the repo URL, only for a public forge (github.com, gitlab.com, codeberg.org...)
- `sync.score_version`: version of the counting rule that produced the numbers
- `sync.to`: end of that window

## About once a minute, while SeaTime runs (health)

The background service checks in with the server, with or without new work. Its owner sees this on the Settings page. The answer carries the settings you chose there and anything you asked this computer to do.

- `health.device_id`: random id of this installation
- `health.client_version`: seatime's version number
- `health.can`: which requests from the Settings page this version can carry out (forget, uninstall, settings)
- `health.tools`: the AI tools SeaTime found on this computer (fields below)
- `health.tools[].name`: the tool's name, such as claude-code
- `health.tools[].state`: whether SeaTime's hooks are in it: hooked, failed or opted-out
- `health.problems`: what is wrong on this computer, as fixed codes (fields below); never an error message, a path or a name
- `health.problems[].code`: one of hooks_silent, hook_failed, sync_behind, sync_failing, no_service, auto_hook_off, no_git, update_failed
- `health.problems[].tool`: the AI tool the problem is about, if there is one
- `health.settings`: the settings in effect on this computer (fields below)
- `health.settings.also_hackatime`: whether editor heartbeats also go to Hackatime
- `health.settings.auto_update`: whether automatic updates are on
- `health.settings.editor_plugin`: whether a WakaTime or Hackatime editor plugin reports to SeaTime

## When this computer finishes a request from the Settings page (result)

- `result.device_id`: random id of this installation
- `result.ok`: whether it worked
- `result.error`: a fixed code when it did not: unknown_project, failed or unsupported
- `result.forgotten`: after deleting prompt text, how many records lost their text (a count)

## When this computer signs in (login)

- `login.device_name`: this computer's name, as the system reports it (its hostname), shown to you on the Settings page
- `login.os`: the operating system, such as darwin or linux
- `login.public_key`: this installation's public signing key; the private key never leaves the computer
- `login.prev_device_id`: only when the server stopped accepting this computer and it signs in again: its old device id, so you can connect the same computer again on the Settings page
- `login.prev_signed_at`: the time of that request
- `login.prev_sig`: a signature of that request with this computer's key, which shows that it comes from the same computer

## Once a stretch of work on a project is over (segment)

About 3 hours after you stop working on a project (90 minutes with nothing new, then 75 minutes for the idle timer's last window to close), SeaTime sends that stretch of the project's records, and the idle-timer records in the same range, so the server can check them against the hashes it received while you worked and count the hours again. Applications you connect read the hours it counts. Every project goes up this way; nothing waits for you to send it. Each record goes in its private form: prompt text, shell commands, file paths, the folder and tool extras are replaced by a commitment (the hash chain needs them to verify; they cannot be read back), and the project is named by its server key. Beside each editor heartbeat on a file inside the project's git repo goes that file's path in the repo, so a segment names files as Hackatime does; files outside the repo, and every path the AI agent touches, stay commitments. Every commitment and prompt fingerprint is an HMAC-SHA-256 under a random key made when SeaTime first ran on this machine. The key never leaves the machine, so nobody can find a short prompt such as "yes" or "fix it" by hashing a word list; two of your own prompts still compare equal. Records of other projects go as a bare fingerprint. Records written by SeaTime versions before this form are not sent at all. Records written before rule `heartbeat/1` may also carry four fields this version never sets (`chars`, `text_hash`, `novelty`, `sim_prev`): they go as they were written, because the hash chain commits to them.

- `segment.claimed`: your totals for the project, as your machine counted them (same fields as a sync bucket)
- `segment.claimed.ai_not_counted_seconds`: your total: seconds of the AI agent's work that did not count
- `segment.claimed.ai_seconds`: your total: counted seconds that ended at the AI agent's heartbeat
- `segment.claimed.coding_seconds`: your total: counted seconds that ended at your own heartbeat
- `segment.claimed.commits`: your total: how many commits
- `segment.claimed.edits`: your total: how many editor heartbeats
- `segment.claimed.harnesses`: your total: names of the AI tools or editors used in that minute, such as claude-code or editor:vscode
- `segment.claimed.models`: always empty: a total is not split by model (the server works the models out from the records)
- `segment.claimed.models[].model`: always empty, as above
- `segment.claimed.models[].prompts`: always empty, as above
- `segment.claimed.models[].seconds`: always empty, as above
- `segment.claimed.project`: your total: project key, which a folder keeps from the first time SeaTime sees it: the public GitHub/GitLab path if the repo was on a public forge then, otherwise local: plus a 12-character hash of the folder name
- `segment.claimed.prompts`: your total: how many prompts you sent (a count, never the text)
- `segment.claimed.runs`: your total: how many test, build or run commands
- `segment.claimed.start`: your total: the minute (UTC)
- `segment.device_id`: random id of this installation
- `segment.entries`: the project's records from your local log, in order
- `segment.entries[].event`: a record of the project (fields below)
- `segment.entries[].event.backfilled`: the record was recovered from the AI tool's own log because its hooks were silent
- `segment.entries[].event.category`: the editor plugin's category for an editor heartbeat, such as coding or ai coding (the same word Hackatime receives)
- `segment.entries[].event.canon`: which chain form the record uses (3: private, with commitments keyed to this machine)
- `segment.entries[].event.command`: never the command: only its keyed commitment
- `segment.entries[].event.cwd`: never the folder: only its keyed commitment
- `segment.entries[].event.deletions`: lines removed by a commit
- `segment.entries[].event.exit_code`: the command's exit code
- `segment.entries[].event.extra`: never the extras: only one keyed commitment of all of them
- `segment.entries[].event.files`: only one keyed commitment of the file paths (the repo-relative name of an editor heartbeat's file goes beside the record, see `segment.entries[].path`)
- `segment.entries[].event.files_changed`: files touched by a commit
- `segment.entries[].event.harness`: which AI tool or editor
- `segment.entries[].event.idle_src`: which idle timer was read (ioreg, win32...) on an idle record
- `segment.entries[].event.insertions`: lines added by a commit
- `segment.entries[].event.kind`: what happened: prompt, agent_start, agent_stop, tool, edit, commit, run, idle...
- `segment.entries[].event.lines`: lines in an editor heartbeat
- `segment.entries[].event.lineno`: the cursor's line number in an editor heartbeat (a number, never the line)
- `segment.entries[].event.cursorpos`: the cursor's column in an editor heartbeat (a number, never the text)
- `segment.entries[].event.model`: AI model name, when the tool reports it
- `segment.entries[].event.new`: whether a prompt was new (4 words or more, not sent before in this project); your machine decides it and keeps the list of sent prompts as keyed fingerprints that never leave it
- `segment.entries[].event.check_hash`: only while the prompt check is on: a salted SHA-256 hash of a prompt that is not a repeat. The salt stays on your machine until the prompt is sent for its check, so the hash cannot be matched against a word list
- `segment.entries[].event.no_input`: start and end times of stretches with no keyboard or mouse input (never what was typed)
- `segment.entries[].event.note`: on a chain-restart record: why the log was restarted (such records are not sent)
- `segment.entries[].event.prev_seq`: on a chain-restart record: the checkpoint the new log continues from (such records are not sent)
- `segment.entries[].event.project`: the project's server key (a public repo path, or local: plus a hash); never the folder name
- `segment.entries[].event.run_kind`: test, build, dev or exec
- `segment.entries[].event.seq`: position in your local log
- `segment.entries[].event.session_id`: the AI tool's session id (random)
- `segment.entries[].event.sha`: commit id
- `segment.entries[].event.text`: never the prompt text: only its keyed commitment
- `segment.entries[].event.tool`: which agent tool ran (Edit, Bash...)
- `segment.entries[].event.ts`: when
- `segment.entries[].event.window_ms`: length of an idle record's window
- `segment.entries[].event.words`: prompt length in words
- `segment.entries[].event.write`: whether the tool wrote a file
- `segment.entries[].event.line_additions`: lines your editor plugin says an edit added (a number; the plugin sends the same to Hackatime)
- `segment.entries[].event.line_deletions`: lines your editor plugin says an edit removed (a number)
- `segment.entries[].event.presence`: how presence was measured on this device, one of four fixed phrases: input+front window, input only, terminal only, not measured
- `segment.entries[].event.input_before_ms`: on a prompt, milliseconds since the computer last saw keyboard or mouse input (never which key or where)
- `segment.entries[].event.terminal_idle_s`: on a prompt from a terminal over SSH or without a desktop, seconds since keys last reached that terminal, in 8-second steps (never which keys)
- `segment.entries[].event.front_window`: on a prompt, only if you switched the front-window check on: yes, no, or no window attached. Never the name of any program
- `segment.entries[].event.session_prompts`: on a prompt, how many prompts the AI tool's session has had so far
- `segment.entries[].event.other_stop_hook`: on a prompt, whether another hook that can re-prompt the agent is installed in the AI tool (yes or no, never which)
- `segment.entries[].event.queued_ms`: on a prompt you typed while the agent was still running, how long the AI tool held it before sending it (milliseconds, from the tool's own log; never the text)
- `segment.entries[].event.input_slices`: on an idle record, for each minute checked every 5 seconds while you worked on a project: the minute and twelve yes-or-no marks, one per 5 seconds, for whether there was keyboard or mouse input
- `segment.entries[].event.front_slices`: on an idle record, only while the front-window check is on: for each minute checked every 5 seconds, the minute and twelve yes-or-no marks for whether the AI tool's own window was in front (never which program was)
- `segment.entries[].event.run`: on an agent stop, counts of what the run did (fields below)
- `segment.entries[].event.run.reads`: files the agent read (a count)
- `segment.entries[].event.run.writes`: file writes the agent made (a count)
- `segment.entries[].event.run.commands`: commands the agent ran (a count, never the command)
- `segment.entries[].event.run.commands_failed`: of those, how many failed
- `segment.entries[].event.run.searches`: searches the agent ran in the code (a count)
- `segment.entries[].event.run.web`: web fetches and searches (a count)
- `segment.entries[].event.run.other`: other tool calls (a count)
- `segment.entries[].event.run.files_inside`: distinct files written inside the project (a count, never names)
- `segment.entries[].event.run.files_outside`: distinct files written outside the project (a count, never names)
- `segment.entries[].event.run.lines_written`: lines the agent wrote (a count)
- `segment.entries[].event.git`: on an agent stop, the repository's uncommitted changes and its current commit (fields below)
- `segment.entries[].event.git.files_changed`: files changed since the last commit (a count)
- `segment.entries[].event.git.insertions`: lines added since the last commit
- `segment.entries[].event.git.deletions`: lines removed since the last commit
- `segment.entries[].event.git.head`: the current commit id (as in `sync.commits[].sha`)
- `segment.entries[].event.prompt_in_log`: on an agent stop, whether the run's prompt was found in the AI tool's own session log (yes or no)
- `segment.entries[].path`: the file's path inside the project's git repo (such as `src/app.go`), only beside an editor heartbeat on a file in the repo; never an absolute path, a folder name above the repo, or a file outside it. Your editor plugin sends the same path to Hackatime
- `segment.entries[].leaf`: fingerprint of a record that belongs to another project (its contents are not sent)
- `segment.entries[].seq`: position in your local log
- `segment.others`: bare times of your counted heartbeats in other projects inside the range: no project, no content. Each gap between heartbeats goes to the project of the one that ends it, so the server needs them to split time as your machine does
- `segment.others_until`: only while the prompt check is on: for each of those bare times, until when it counted (a time, no content), so the server splits time as your machine does
- `segment.project`: project key of the project
- `segment.score_version`: version of the counting rule
- `segment.sig`: signature over the bundle with this device's key
- `segment.start_hash`: chain fingerprint just before the first record sent

## Only while the server's prompt check is on, when an AI turn ends (check)

Sent once per prompt that is not an exact repeat in its project, when the AI stops, you send the next prompt, or the session ends. If it cannot be sent it waits on your machine and is tried again later. The server keeps the answers and the hash, never the text.

- `check.device_id`: random id of this installation
- `check.check_hash`: the prompt's salted hash, the same one your log recorded when you sent the prompt
- `check.salt`: the random salt behind that hash, so the server can check that the text matches what your log recorded
- `check.turn`: the turn being checked (fields below)
- `check.turn.prompt`: the prompt text, sent for the check, never kept (at most 8 KB)
- `check.turn.prev_reply`: the AI's reply just before this prompt, sent for the check, never kept (at most 4 KB)
- `check.turn.diff`: a short excerpt of what changed in the project during this turn: the `git diff` of each file the turn changed, and new files' first lines; sent for the check, never kept (at most 6 KB)
- `check.turn.activity`: one line of numbers from your log: time since the AI finished, keyboard/mouse time, how many commands you ran and of which kind (test, build, dev or exec, never the command), how many hand edits
- `check.turn.check_hash`: not sent for the judged turn (its hash is `check.check_hash`)
- `check.turn.salt`: not sent for the judged turn (its salt is `check.salt`)
- `check.context`: up to 6 earlier turns of the same project, in any chat, as context for the check (same fields below), never kept
- `check.context[].prompt`: an earlier prompt's text, in full as it was hashed (at most 8 KB), so the server can check it against its hash; the server cuts it to 2 KB before the check
- `check.context[].prev_reply`: the AI's reply before that prompt (at most 2 KB), or a note that it opened a new chat
- `check.context[].diff`: that turn's change excerpt (at most 2 KB)
- `check.context[].activity`: that turn's activity line
- `check.context[].check_hash`: that prompt's salted hash, the same one your log recorded; the server keeps it with the verdict (a hash, no text), so the server can show which turns the check saw
- `check.context[].salt`: the salt behind it, so the server can check the text; never kept
- `check.earlier`: up to 20 (the server's setting) earlier prompts of the same project that passed their check, oldest first, text only, so a reworded repeat is seen as one; never kept
- `check.earlier[].prompt`: the prompt's text as it was hashed; the server cuts it to 1 KB before the check
- `check.earlier[].check_hash`: its salted hash, kept with the verdict like the context's
- `check.earlier[].salt`: the salt behind it; never kept
- `check.earlier[].prev_reply`: never filled for an earlier prompt
- `check.earlier[].diff`: never filled for an earlier prompt
- `check.earlier[].activity`: never filled for an earlier prompt
- `check.project`: what your machine read in the project's folder when you sent the prompt, so the check can tell whether the prompt is about this project (fields below); sent for the check, never kept. For your home folder only the name is sent
- `check.project.name`: the project's name: the folder's name, or a public repo's
- `check.project.files`: names of files in the project, by their path inside it (at most 2 KB, the ones nearest the top first). In a git repo these are the files git tracks or would track, so nothing your `.gitignore` leaves out; vendored and build folders are skipped. Names only, never what is in a file
- `check.project.total`: how many files the project has (a count)
- `check.project.readme`: the start of the README in the project's top folder, if there is one (at most 2 KB), with secrets blanked as in prompts
- `check.git_ins`: lines added since the last commit, as git counted them when the AI stopped (the same number your log holds)
- `check.git_del`: lines removed since the last commit, as above
- `check.git_known`: whether those two counts were logged (not when you stopped the AI halfway)
- `check.prompt_ts`: when you sent the prompt (the same time your log holds), so the minutes it covers can be counted even when the answer comes back late
- `check.front_measured`: whether your machine measured how long the AI tool's window was in front (yes or no); if not, the check skips the question that needs it
