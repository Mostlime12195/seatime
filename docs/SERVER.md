# seatimed (server)

seatimed is one static Go binary (`cmd/seatimed`), built without cgo. It keeps its data in Postgres in production,
or in a SQLite file for local use and tests. Pages use `html/template` with embedded static assets. Live updates use SSE. Code:
`internal/server` (HTTP, auth, UI) and `internal/server/store` (the same code runs on Postgres and SQLite).

## Run locally

```sh
go build -o bin/seatimed ./cmd/seatimed
SEATIME_DEV=1 SEATIME_ADDR=127.0.0.1:8790 SEATIME_DB=/tmp/seatime.db ./bin/seatimed
# open http://127.0.0.1:8790/login and use the dev form (any username; pick admin to try /admin)
```

`SEATIME_DEV=1` turns on the dev login form and makes a cookie key that lasts until the process stops.

## Configuration (env)

- `SEATIME_ADDR` is the listen address (default 127.0.0.1:8790; the container image sets `:8080`). `SEATIME_BASE_URL` is the public URL that people type. Set `SEATIME_TRUST_PROXY=1` behind a proxy (the image sets it).
- `SEATIME_DB`: a `postgres://` or `postgresql://` URL, or the path of a SQLite file (default `seatime.db`). See "Database" below.
- `SEATIME_SECRET`: the cookie key, e.g. from `openssl rand -hex 32`. Required unless `SEATIME_DEV=1`. See "The cookie key".
- Hack Club Auth (OIDC, authorization code + PKCE, discovery from the issuer): `SEATIME_HCA_CLIENT_ID`,
  `SEATIME_HCA_CLIENT_SECRET`, `SEATIME_HCA_ISSUER` (default https://auth.hackclub.com), `SEATIME_HCA_SCOPES`
  (default `openid profile slack_id`; drop `slack_id` if the issuer rejects it).
  Redirect URI: `$SEATIME_BASE_URL/auth/hackclub/callback`.
- GitHub OAuth: `SEATIME_GITHUB_CLIENT_ID/SECRET`, callback `$SEATIME_BASE_URL/auth/github/callback`.
- `SEATIME_HACKATIME_TRUST=1` (off by default) asks Hackatime for each account's trust colour, by the account's
  Slack ID. The server serves that colour on `trust_factor`. It caches the colour for 15 minutes. If Hackatime stops
  answering, the last colour that it served is kept for 7 days. `SEATIME_HACKATIME_URL` sets the address (default
  https://hackatime.hackclub.com). This option sends Slack IDs to Hackatime. While it is on, the account's /settings
  page shows a line about it. It never changes hours.
- `SEATIME_COUNT_EVERY` (default `1h`) is how often the server counts the records that devices send. It counts at
  every multiple of this on the clock, so `1h` is each hour on the hour and `15m` is each quarter of an hour. The
  pages say "checked on the hour", or "checked every 15m".
- `SEATIME_REVIEW_KEYS` (none by default) is a comma-separated list of keys, each 32 characters or more, for Hack
  Club's review tool. A request with `Authorization: Bearer <key>` may call
  `GET /api/v1/review/users/{user}/projects/{project}?since=&until=`. `{user}` is a Slack member ID or a username, and
  `{project}` is a name, slug or key. The answer holds the project's counted hours and status as applications read them,
  and the checked records of each segment (`pkg/api/review.go` lists every field): times, kinds, word counts, the prompt
  check's answers, input and front-window slices, what each agent run did, and commits. It never holds prompt text, a
  command, a folder, or a hash of them. A project that its owner hid answers 404. The key reads any other project, on
  a private profile too, so give it only to a tool that checks who is looking. With no key set, the address answers 404.
  `GET /api/v1/review/users/{user}` lists the account's projects that are not hidden, each with its hours and the times
  of its first and last checked record. `GET /api/v1/review/users?q=` finds accounts by a part of a username, display
  name, Slack member ID or GitHub login.
- Prompt check (docs/RULE.md, "When the AI check is on"; `pkg/promptcheck`). It is off by default. Its switch and its policy are
  settings (see below). The first two items below are set only in the environment.
  - `OPENROUTER_API_KEY` is the API key. It is never logged or shown. If the check is on and there is no key, every
    check answers 503 with Retry-After. Devices then keep the prompt in their queue, and its time stays pending until
    a key is set. Nothing is decided in the meantime.
  - `SEATIME_PROMPT_CHECK_URL` is the address of the decisions API. It can also be a comma-separated list of
    addresses that the list of judges may name. The key is sent to these addresses, so they are never settings.
  - How it works: every device keeps every prompt that it records in its local database, with the check on or off.
    It keeps the text, a salt and the turn around the prompt, and it never drops a prompt. The salted hash of each
    prompt goes at once to `POST /api/v1/prompt-hashes`. The hashes are chained for each device
    (`pkg/promptcheck/chain.go`, table `prompt_chain`), so that a later edit, deletion or change of order can be
    seen. Devices learn the switch and the account's own setting from the answer to a sync. While both are on, a
    device sends each finished turn to `POST /api/v1/prompt-check`. With it, the device sends the project's turns
    before it (in any chat) and the project's earlier prompts that passed. It also sends what the hook read in the
    project's folder when the prompt was sent: the project's name, some of its file names and the start of its
    README (`promptcheck.Project`). The judge reads these in a PROJECT section. A device older than v0.1.8 sends no such facts and is judged on the session alone. The
    server checks every text against its hash and asks the judges in order. It keeps only the yes/no labels (null for a question that was not asked), the
    hash, the model, the rules version and the minutes. It also keeps what the prompt was judged on, stored as
    numbers and prompt hashes. It drops the text and the project facts. The server sends a verdict's minutes to the device, and the device
    counts with them.
  - The rule that applies depends on when the work happened. Each change of the switch is recorded in
    `rule_periods`. Each change of an account's own setting (in Settings, on by default) is recorded in
    `user_check_periods`. Both are sent to devices. Time while either one was off counts with `heartbeat/1`. Time
    while both were on counts with `heartbeat/2` (`pkg/ledger/cap.go`). A stretch when an account had its setting off
    switches to `heartbeat/2` once every prompt in it is settled, either with a verdict or closed without one.
    Devices and the server work this out the same way (`promptcheck.Effective`). An older device keeps no chain, so
    it never switches a stretch.
  - Pulls: the server can ask an account's devices for their saved prompts (`prompt_pulls`). On their next sync,
    devices send those prompts to be checked, whatever the account's setting is. The server checks each text against
    its chained hash and keeps the verdicts. If the text does not match, the prompt is closed as `mismatch`. If it
    has not arrived by the deadline, it is closed as `missing`. Both count as not passing.
    `seatimed prompts pull -user NAME -project KEY` asks for what is needed to ship that project. That means the
    project's prompts. If one of them falls in a stretch when the account had the check off, it also means every
    prompt in that stretch. `seatimed prompts pull -user NAME -from T -to T [-project KEY]` asks for a time range.
    `seatimed prompts status -user NAME` shows how far they got. These commands open `SEATIME_DB` directly.
- `SEATIME_TRUSTED_HOPS` (default 1) is used with `SEATIME_TRUST_PROXY=1`. It is the number of proxies that append to
  `X-Forwarded-For`. The client address is taken that many entries from the right, so that nothing a client writes is
  used. On Orchard it is 2: the ingress, then the ingress pod. If the address found is private, the client's address
  was lost before it reached these proxies, for example because a load balancer rewrote it. Then the limits for each
  address share one bucket that is 50 times larger. The cap on live connections for each address is shared the same way.
- `SEATIME_DL_DIR`: directory of release binaries served at `/dl/` (see "One-line install" below).
- `SEATIME_ADMIN_IDS` is the default of the setting `admin_ids`. It lists login identities as `provider:subject`:
  `hackclub:<HCA sub>` (alias `hca:`), `github:<numeric user id>`, `slack:<Slack member id>`, or `dev:<name>` with
  `SEATIME_DEV=1`. A `slack:` id matches only on a Hack Club Auth login, because Hack Club Auth confirms it.
  seatimed logs `"msg":"login" ... "identity":"hackclub:..."` at every sign-in. So have the person sign in once,
  and copy the id from the log. The admin role always follows the list, both ways. The list is applied at every
  login, at start, and whenever an admin edits it on /admin. Someone who is no longer listed loses the role at once
  and is signed out. Someone newly listed gets the role without signing in again. Usernames never give a role,
  because anyone can pick one.
- `SEATIME_DEBUG=1` serves `/debug/stats` (counters for the write lock, the heap and SSE) and `/debug/pprof/`, for
  capacity testing. They are served on a separate listener at `SEATIME_DEBUG_ADDR` (default `127.0.0.1:6060`), which
  must be a loopback address. They are never on the public port. It is off by default. `seatimeload run -debug-url`
  reads it.
- With `SEATIME_DEV=1`, seatimed refuses to start if `SEATIME_BASE_URL` is a public https URL, because dev mode lets
  anyone sign in as anyone.

## Database

In production, create an empty Postgres database and set `SEATIME_DB` to its URL. seatimed makes the tables when it
starts. For local use, set `SEATIME_DB` to a file path and seatimed uses SQLite.

- Run only one seatimed per database. Two copies could mix up some writes.
- seatimed refuses to start on a database that a newer seatimed has already set up.
- Back up the database with your Postgres host's own backups.
- New tables and columns go in `internal/server/store/migrations.go`, as a new entry at the end of the list.

The tests use SQLite. To run them on Postgres, use a user that can create databases:

```sh
SEATIME_TEST_PG='postgres://user@localhost:5432/postgres?sslmode=disable' go test ./internal/server/...
```

Each test makes its own database and drops it at the end.

## Settings (changed on /admin, no redeploy)

Policy values are kept in the `settings` table. Admins edit them on `/admin`, with a CSRF check. Each one takes its
default from the environment variable named in the table below. So with no saved values, seatimed behaves exactly as
the environment says. A value saved on /admin overrides the default until it is reset. Every change is logged in
`settings_log` with who made it, and shown on /admin. If a stored value no longer parses, seatimed logs it and uses
the default. Secrets and addresses stay in the environment.

| Setting | Env default | Default | What it does |
|---|---|---|---|
| `prompt_check` | `SEATIME_PROMPT_CHECK=1` | off | the prompt check's switch; each change is recorded as a rule period |
| `prompt_check_models` | `SEATIME_PROMPT_CHECK_MODEL` | `liquid/d1.2-ultra` | judges, one per line, tried in order; a line may add an address from `SEATIME_PROMPT_CHECK_URL` |
| `prompt_check_rules` | `SEATIME_PROMPT_CHECK_RULES` (the JSON itself) or `SEATIME_PROMPT_CHECK_FILE` (a JSON file) | one plain question, 10 minutes | questions, effort/block/need, yes threshold, kinds with minutes, request caps (`pkg/promptcheck` Rules); each verdict records the rules ID and its minutes |
| `prompt_check_budget_usd_day` | `SEATIME_PROMPT_CHECK_BUDGET_USD_DAY` | 25 | paid calls stop for the rest of the UTC day at this cost; checks wait |
| `prompt_check_user_day` | `SEATIME_PROMPT_CHECK_USER_DAY` | 600 | paid calls per account per UTC day |
| `prompt_check_user_per_min` | `SEATIME_PROMPT_CHECK_USER_PER_MIN` | 10 | paid calls per account per minute (bursts of three minutes' worth) |
| `prompt_check_concurrency` | `SEATIME_PROMPT_CHECK_CONCURRENCY` | 8 | paid calls in flight across everyone |
| `prompt_check_timeout_s` | `SEATIME_PROMPT_CHECK_TIMEOUT_S` | 120 | one judge call; the request's write deadline and the devices' wait follow it |
| `prompt_check_late_days` | `SEATIME_PROMPT_CHECK_LATE_DAYS` | 14 | how far back a late verdict raises synced minutes and devices recount |
| `prompt_check_queue_days` | `SEATIME_PROMPT_CHECK_QUEUE_DAYS` | 30 | sent to devices; current devices ignore it and keep every prompt (older ones dropped a prompt's text after this long) |
| `prompt_check_earlier` | `SEATIME_PROMPT_CHECK_EARLIER` | 20 | earlier passed prompts of the project shown to the judge |
| `max_devices` | `SEATIME_MAX_DEVICES` | 10 | devices per account; connecting another is refused |
| `min_client_version` | `SEATIME_MIN_CLIENT_VERSION` | none | older devices are told to update; their syncs wait and are recounted after the update, their checks and segments answer 426 |
| `admin_ids` | `SEATIME_ADMIN_IDS` | none | see above |

Once any spending limit is reached, a check answers 429 or 503 with Retry-After. The device keeps the prompt in its
queue, and its time stays pending. Nothing is decided or lost. /admin shows these as plain text: whether the check is
on and has a key; the order of the judges; the last judge answer and the failures in the last hour; today's calls and
spend against the budget; the time waiting across all accounts; and the devices of the last 7 days, by SeaTime version.

**Turning the check on:** first ship the client release. Then watch the versions on /admin until the devices have it,
or set `min_client_version`. Then switch the check on. Work recorded before the switch keeps counting with `heartbeat/1`.

## The cookie key

`SEATIME_SECRET` signs the session cookies. seatimed does not start without it, unless `SEATIME_DEV=1`: then it makes
a random key that lasts until the process stops.

Each session also carries its account's session epoch. Logging out, or a change of role, ends every session of that
account, so a copied cookie stops working too. Changing the key signs everyone out.

## Health and shutdown

- `GET /healthz` does one small database read, with a time limit of one second. It answers 200 when the database answers. It answers 503 when the database does not answer, or once shutdown has begun. It needs no login, and the response holds no data.
- On SIGTERM, `/healthz` starts to answer 503. Requests in flight get up to 15 s to finish. Then the store closes; on SQLite, it first checkpoints the write-ahead log. Give the process a grace period of at least 20 s.
- Logs are JSON lines on stdout.

## Deploy on Orchard

Orchard builds the `Dockerfile` at the repository root from GitHub and runs it on Kubernetes.

- **Image.** A two-stage build: a static binary on a distroless base, without cgo. It sets `SEATIME_ADDR=:8080` and `SEATIME_TRUST_PROXY=1`.
- **Database.** A Postgres database. Set `SEATIME_DB` to its URL. The pod needs no volume.
- **One replica.** Never scale past one, and use a deploy strategy that stops the old pod before it starts the new one.
- **Port.** The service port is 8080.
- **Health check.** Path `/healthz`, and **set the health-check port to 8080 explicitly**. Do not rely on a default: a probe on the wrong port keeps a healthy pod out of rotation.
- **Login in front.** While SeaTime is not public, turn on Orchard's login protection on the ingress. These paths must bypass it:
  - `/.well-known/acme-challenge/`, or the certificate can never be issued or renewed and the site ends up with no valid TLS.
  - nothing else, for a closed pilot used in the browser. But the `seatime` client cannot get past a browser login. So before any client syncs from outside, either turn the protection off, or let `/api/`, `/install.sh`, `/install.ps1` and `/dl/` bypass it too.
- **Environment to set.** `SEATIME_DB` and `SEATIME_SECRET` (both secret), `SEATIME_BASE_URL` (the public https URL), the Hack Club Auth and GitHub client ids and secrets, `SEATIME_ADMIN_IDS`.
- **Grace period.** 20 s or more.
- **After the first deploy, check** that `/healthz` answers 200 through the ingress, and that a login survives a redeploy.

## One-line install

seatimed serves the installers and the binaries they fetch:

- `GET /install.sh` (macOS/Linux, POSIX sh) and `GET /install.ps1` (Windows, PowerShell 5.1-compatible). Both are
  filled in with `SEATIME_BASE_URL`. It must be the public URL that users type, in the plain form
  `http(s)://host[:port][/path]`. Otherwise seatimed answers 500, rather than put the value into a script. The landing
  page shows the matching commands.
- `GET /dl/seatime-{windows,darwin,linux}-{amd64,386,arm64}[.exe]`, `GET /dl/SHA256SUMS` and `GET /dl/RELEASE` from `SEATIME_DL_DIR`.
  Only those names are served: there is no directory listing, and no other file is served. Without `SEATIME_DL_DIR`, they
  answer 404, and the installers fail with a clear message. If `SEATIME_DL_UPSTREAM` is set (a plain http(s) URL, e.g.
  `https://github.com/OWNER/REPO/releases/latest/download`), `/dl/<name>` redirects there instead, and the server holds no binaries.

Publishing a release: do it on a machine that is not the server. The signing key never goes to the server.

1. Once: `go run ./cmd/seatime-release keygen <path>` writes the private key (mode 600) and prints the public key. Put the public key
   in `internal/client/update/key.go` and commit it. Until then, clients refuse to update themselves. Clients accept a manifest
   signed by any key in `PublicKeys`. To rotate keys, add the new key there, and ship a release signed with the old one. Then
   sign with the new one, and drop the old one. `seatime-release sign -expires 720h` adds an expiry date.
2. `git tag vX.Y.Z && SEATIME_SIGNING_KEY=<path> VERSION=vX.Y.Z scripts/release.sh` builds the six targets into `dist/`. It
   writes `SHA256SUMS` and the signed `RELEASE` manifest. It refuses to run if the key does not match `key.go`.
3. Copy `dist/*` into `SEATIME_DL_DIR` (`RELEASE` last), or upload it as release assets where `SEATIME_DL_UPSTREAM` points.

Installed clients check `RELEASE` once a day. They verify its ed25519 signature and the SHA-256 of each file, then swap in the
new binary. If `seatime doctor --self-test` fails, they roll back. The installers themselves check only `SHA256SUMS`, so a
first install relies on TLS to your server. Every update after that has its signature checked.

The installers need no sudo or admin rights. `install.sh` writes `~/.local/bin/seatime` (set `SEATIME_INSTALL_DIR` to change
this). It adds a PATH line to your shell rc file (skip with `SEATIME_NO_MODIFY_PATH=1`). Then it runs
`seatime setup --yes --server <url>` (skip with `SEATIME_NO_SETUP=1`). `install.ps1` installs to
`%LOCALAPPDATA%\SeaTime\bin` and changes only the *user* PATH.

## Surface

API (see `pkg/api`): `POST /api/v1/device/start|poll`, `POST /api/v1/sync`,
`POST /api/v1/device/check-in` (a device's health; answers with the account's settings and requests from the web),
`POST /api/v1/device/requests/{id}` (a device's answer to one request), `POST /api/v1/records` (a device's records,
as it writes them), `POST /api/v1/segments` (a stretch of records, from a device older than v0.1.14),
`GET /api/v1/users/{u}/projects/{p}/stats?since=&until=` and `.../ysws[.txt]` (a project's checked hours, status and
export; with the user's own token, `stats` also has the checked time and prompts by AI model, see `YSWS.md`), the read API in Hackatime's format (see `YSWS.md`), OAuth (`GET|POST /oauth/authorize`, `POST /oauth/token`,
`POST /oauth/revoke`), and `GET /healthz`.
Pages: `/`, `/leaderboard`, `/@user`, `/@user/<project>?day=YYYY-MM-DD`, `/settings` (plus `POST /settings/project/visibility`, the hide switch for a newly synced project;
`POST /settings/computers`, the settings every computer applies; `POST /settings/forget` and
`POST /settings/device/remove`, requests a computer carries out at its next check-in; `POST /settings/applications/revoke`),
`/device`, `/developers` (applications a user registered), `/admin` (admins).
Live: `/live/global` and `/live/@user` (SSE). They send a heartbeat every 20 s. Each client has a buffer of 8 messages,
and messages are dropped when it is full. There can be 3000 connections in total and 20 per IP.
On each SSE event, profile and project pages fetch their server-rendered block again (`?frag=1`).

Time by model (`models.go`, `store/models.go`; docs/RULE.md, "Time by model"). A synced minute names the models behind
its AI seconds and prompts (`buckets.models`). `hourly_models` adds them up per user, project and hour, next to
`hourly`, and its rows always add up to `hourly`'s counted seconds. The profile and project pages show this to the
owner only, as rows of counted time and prompts: one row per model, one for hand time and one for AI time with no model
on record. A page shows a model under the name the AI tool reported, without a trailing release date. A checked
segment's minutes are split the same way in `segment_models`, counted by the server from the segment's records. That
is what `stats` serves to applications, with the names unchanged. Time synced or checked before migration 7 has no
model on record.

Which project a record belongs to (`store/projectkeys.go`; on the device, `internal/client/store/folders.go`). A
project is a folder on a computer. The computer gives a folder a key the first time it sees it and keeps that key
whatever happens to the folder's git remote. The remote is the folder's label, and a sync sends both. `project_keys`
holds every key each device has sent and the project it belongs to. That never changes, so a record counts for the same
project on every replay. A key a device sends for the first time joins the project that goes by its label now
(`projects.key`), or starts one. When a device's label for a key changes, the project goes by the new label, unless
another project already does: then both stay as they are, and no project takes in another's time. A project's slug
does not change, and `project_links` keeps its earlier keys leading to it. Migration 8 folded, once, the projects of one
account that held the same commit: an older device made a new project each time a folder's remote changed, and
recorded the folder's commits under each.

A project with no counted time, no checked time and no commits is not listed on the profile or in Settings. Its own
page still answers.

## Trust model (what the server checks)

Sync: the bearer token must belong to a device, and `device_id` must match it. The checkpoint signature
(`seatime-checkpoint\n<device>\n<seq>\n<hash>`) must verify with the key that the device registered. Checkpoints
are append-only. Sending the same checkpoint again is idempotent. A checkpoint with the same seq but a different
hash, or with a seq lower than the latest one, is rejected with 409. Buckets are validated: each must be aligned to
a minute, not in the future, and count at most 60 s. They are stored by user, project, minute and device, so
sending the same minutes again never counts them twice.

Records: after each sync a device sends the part of its chain that the server does not hold yet, up to the
checkpoint of that sync (`POST /api/v1/records`, `internal/client/records`). Each record of a project and each
idle-timer record is in its private form. Everything else is a leaf hash. The server replays the entries from
`start_hash` and keeps them only if they end at a stored checkpoint and match every stored checkpoint on the way. A
SHA-256 chain that ends at a known hash cannot be made up backwards, so what the server keeps is the chain that the
device signed while it worked. This holds wherever the entries start, so a device that lost part of its log can go
on after a hole. A record that is not in the private form is kept as its leaf hash only. Tables: `records` (one row
per seq) and `record_streams` (how far each device's records go).

Counting: the server counts at every mark of the clock (`SEATIME_COUNT_EVERY`, `countLoop` in
`internal/server/records.go`). One server counts at a time. A mark that was not finished, because the server
stopped, is counted when a server next looks. For each device that sent records since the last mark, the server
cuts each project's records into stretches of work, the way a device used to (`pkg/audit` Split and Over): a
stretch ends in the middle of a pause of 3 hours or more, about 3 hours after its last record. Each stretch is one
segment. The last stretch of a project is open. It is counted again at every mark with the records that came since,
and that is why AI time shows within the hour. A stretch that is over is counted again only when its prompts were
waiting for their check, or when records with a time inside it arrive late. When a stretch comes to hold minutes
that a segment made after it also holds, that later segment is counted again and gives them up, so that no minute
counts twice. Nothing about checked time changes between two marks.

Segments: a segment is one stretch of one project's records, as a bundle (`pkg/api` Bundle). The server builds it
from the records above. A device older than v0.1.14 uploads it itself (`POST /api/v1/segments`), about 3 hours
after the last record, with an ed25519 signature over the raw 32-byte `sha256(json(bundle with sig=""))`. Either
way the server checks six things. There is at least one stored checkpoint inside the seq range. `chain.Verify`
passes, so every stored checkpoint in the range matches. `start_hash` equals the stored checkpoint at `seq_lo-1` (or
genesis at seq 1). There are no entries after the last stored checkpoint. No event is timestamped more than 5 min
after the server first received a checkpoint that covers it (this stops backfill). The records span at most 62
days. The server counts the records itself, with the rule in docs/RULE.md. If a device counted them differently,
that is not a problem. Any failed check is listed in `problems`, and the segment fails. A failed segment credits
nothing, and the project shows `failed` with the reason. The server also fails a chain that a device of another
account sent as well (a copied device key). It takes out minutes that an earlier segment of the same account
already credits. An uploaded segment whose prompts are waiting for their check is `pending`. It is counted again,
on a read and every 3 minutes, until the checks are in. Tables: `segments`, `segment_blobs` (the bundle, gzipped)
and `segment_minutes` (credited seconds per minute; this is what applications read).
`POST /api/v1/submissions` answers 410 for clients from before segments.

Applications (OAuth 2.0, `oauth.go`): an application must use the authorization code flow with PKCE S256. The
redirect must match exactly, and `state` is required. A code can be used once, within 60 s. Access tokens last 1 h.
Refresh tokens last 30 days and are rotated. If a code or refresh token is used twice, its whole family is revoked.
Client secrets, codes and tokens are stored as SHA-256 hashes. There is one scope, `read`. An access token can read
its user's private profile, but not hidden projects. The first time an application reads a project, the server asks
the account's devices for the prompts from stretches when the account had the check off (`ShipPull`, once per
project). The project is pending until those prompts are checked. It fails if they are still missing after 14 days.

Privacy: profiles are public by default. Only the owner and the applications that they approved can read a private
profile. Hidden projects are never listed or served to anyone else, but their time counts in totals and on the
leaderboard. No prompt text, shell command or folder path reaches the server in a sync or in the records. A sync names a project
that is not a public repo by its folder's name, until the owner gives it another. The records
name only the files edited inside the project's git repo, by their path relative to the repo. The editor
plugin sends Hackatime those paths anyway. Records are served only to a review tool that holds a review key
(`SEATIME_REVIEW_KEYS` above).

## Git integration

A project's repo comes from `ProjectMeta.Remote` or a `github.com/...` key, or someone sets it by hand in Settings.
Commit SHAs from syncs and segments go into the `commits` table. The server does not ask GitHub about them, and
the API serves them as the devices sent them.

The project page lists the 15 newest commits of the last 30 days. The viewer's browser asks GitHub's public API
about them (`web/static/commits.js`), without a token, and shows "on GitHub", "not found" or "repo not public" next
to each. One call, `GET /repos/{repo}/commits?per_page=100`, covers the default branch. Each listed commit that is not
in that answer costs one call more, `GET /repos/{repo}/commits?sha={sha}&per_page=100`, which also covers 99 of its
ancestors. A page view makes at most 5 calls, since GitHub allows such a browser 60 an hour. The browser remembers
a commit that was found (localStorage) and does not ask about it again. It keeps "not found" and "repo not public"
for 10 minutes. When GitHub does not answer, the badge says "not checked". The page's Content-Security-Policy lets
scripts reach `https://api.github.com` and no other host.

The `verified`, `message` and `author` columns of `commits` are left from the time when a server worker did this
check. Nothing reads or writes them.

## Known limits / future work

- The leaderboard uses UTC windows (today / 7 d / 30 d). A profile's "today" and its streaks use the user's UTC offset in whole hours.
- Hack Club Auth login uses the userinfo endpoint instead of checking id_token signatures. The token is fetched directly from the issuer over TLS.
