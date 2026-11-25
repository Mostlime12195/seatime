# SeaTime

SeaTime is an experimental tracker. It counts the hours that you spend building software with AI tools: typing prompts, reading what the agent did, steering it, and editing code by hand. The time that an agent works counts only while you are there. Asking for the same fix again and again counts for less. The whole rule fits on one page: [docs/RULE.md](docs/RULE.md).

SeaTime runs on your computer and sends your hours to a SeaTime server. You see them on that server's website.

## What it works with

- **AI tools for coding**, through their hooks or their logs. Time goes to the repository that the tool runs in. Claude Code, Cursor and Gemini CLI have been tested with real installs. Support for the other tools was written from their documentation and has not been tested live yet: Codex CLI, opencode, GitHub Copilot, Windsurf, Antigravity, Kilo Code, Aider, Cline, Roo Code and Amp. The status of each tool is in [docs/ADAPTERS.md](docs/ADAPTERS.md).
- **Editors**, through the WakaTime or Hackatime plugin that you already use.
- **Anything else**, through a local HTTP endpoint. See [PLUGINS.md](PLUGINS.md).

The AI model and the tool that you use never change your hours, so free tools count the same as paid ones.

## Install

Copy the line for your system from your SeaTime server's home page and paste it into a terminal. It looks like this:

```sh
# macOS / Linux
curl -fsSL https://seatime.example.com/install.sh | sh
```

```powershell
# Windows (PowerShell 5.1 or newer)
irm https://seatime.example.com/install.ps1 | iex
```

You do not need admin rights. The installer checks the SHA-256 checksum of the download. Then it:

- adds SeaTime's hooks to every AI tool that it finds;
- starts SeaTime in the background, and again each time the computer starts;
- connects your WakaTime or Hackatime editor plugin, as described under Privacy;
- opens your browser so that you can sign in.

That is the last step you take in a terminal. Everything after it happens on its own or on the website.

## Using it

Code as usual. Your hours show on your profile on the website within a few minutes.

SeaTime looks for new AI tools every 10 minutes and adds its hooks to them. If an update of a tool removes the hooks, SeaTime puts them back. If you used a tool while its hooks did not work, SeaTime fills in that time from the tool's own log.

The Settings page on the website lists your computers. For each one, it shows when it last synced, its SeaTime version, the AI tools that it found, and any problem. If a computer is signed out, SeaTime opens the sign-in page in your browser. The Settings page also shows a button to approve that computer.

A project that is not a public repo shows with a code instead of a name until you name it. You can name it on the Settings page.

## Settings

These are on the Settings page. Each computer applies them within a minute or two.

- **Also send editor time to Hackatime.** On at first. See Privacy.
- **Update SeaTime automatically.** On at first. SeaTime installs only releases that are signed with the SeaTime release key. If a new version does not work on your computer, SeaTime goes back to the previous version.
- **Delete prompt text on all my computers.** See Privacy.
- **Remove SeaTime from this computer.** This takes SeaTime's hooks out of your AI tools, gives your editor plugin its old settings back, and stops SeaTime on that computer. You can also delete SeaTime's data there.
- **Delete my SeaTime account.** This deletes your projects, hours, computers and prompt checks from SeaTime, and the applications that you made or allowed. Your computers then remove SeaTime by themselves. Your Hack Club account stays as it is.

## Privacy

Your prompts and shell commands stay on your computer, and so do file paths. A sync sends your totals for each minute and a hash of the log on your computer. A project is a folder, and it stays the same project when you add a git remote to it or the repo moves. A project that is not a public repo goes by the name of its folder: SeaTime sends that name, never the path to the folder. You can rename or hide a project on the Settings page. Each computer also sends records so that the server can check your hours. In those records, prompts, commands, folders and paths are only keyed hashes, and nobody can read them back. The one exception is the files that you edit inside the git repo of the project. They are sent with their path inside the repo, the same way that your editor plugin already sends them to Hackatime. [pkg/api/WIRE.md](pkg/api/WIRE.md) lists every field that is sent.

SeaTime keeps the text of your prompts on your computer for the prompt check ([docs/RULE.md](docs/RULE.md)). While the check is on, an AI model reads each new prompt with the chat and the code change around it, and with the project's name, some of its file names and the start of its README. The server passes these on for the check and stores none of them. You can delete the prompt text from the Settings page. The hashes stay, so your hours and the records that the server checks do not change. A prompt that you send again also still counts as a repeat. A prompt without its text cannot be checked. So if a prompt is still waiting for the check, or if the server asks for it later, its time does not count.

If your editor has the WakaTime or Hackatime plugin, SeaTime reads its heartbeats and passes them on to Hackatime unchanged, so your Hackatime stats stay the same. You can turn that off on the Settings page. Data from AI tools is never sent to Hackatime.

## Applications

Applications, such as the site of a Hack Club YSWS program, read your hours from SeaTime the way that they read them from Hackatime. The first time that an application connects, SeaTime shows you what it would see (your project names, hours and commit list). Then it asks you whether to allow it. You can take that permission back in Settings. You do not need to submit anything.

Each computer sends the records of a project about 3 hours after you stop working on it. The server checks them against the hashes that your computer sent while you worked. Then it counts the hours again. Applications read those checked hours, with a status: verified, pending (not checked yet) or failed (some records did not match). [YSWS.md](YSWS.md) is for people who build applications.

## Running a server

`seatimed` is one Go binary. It keeps its data in Postgres, or in a SQLite file when you run it on your own computer. [docs/SERVER.md](docs/SERVER.md) covers setup, deployment and releases.

## Contributing

You need Go 1.27 or newer; cgo is not used.

```sh
go build -o bin/seatime ./cmd/seatime     # client
go build -o bin/seatimed ./cmd/seatimed   # server
go test ./...
```

To add support for another AI tool, read "Writing a new adapter" in [docs/ADAPTERS.md](docs/ADAPTERS.md). The counting rule is in `pkg/ledger`. A test checks that the numbers in docs/RULE.md match it.

## Command line (optional)

You never need these. Most of them do the same thing as the website, or as something that SeaTime does on its own. The rest are for people who want to look closer.

- `seatime today`, `seatime dash`: your hours, in the terminal or on a page served by your own computer.
- `seatime status`, `seatime doctor`: what SeaTime is doing, and a check that also repairs hooks.
- `seatime login`: sign this computer in again.
- `seatime name <project> <name>`: name a project. A name set on the website takes priority.
- `seatime setup --also-hackatime=false`, `seatime update off`, `seatime forget`, `seatime uninstall`: the same as the settings above. `seatime update` installs the newest release now. `seatime uninstall --harness claude-code` removes the hooks from one tool, and SeaTime does not add them back.
- `seatime setup --yes --no-wakatime`, `--no-service`, `--no-login`: install without those steps.
- `seatime inspect`: print exactly what the next sync would send (`--segment <project>`: a project's records as they are sent for checking).
- `seatime emit`, `seatime token`: for plugins ([PLUGINS.md](PLUGINS.md)).

A build that you compile from source never updates itself.

## License

AGPLv3. See [LICENSE](LICENSE).
