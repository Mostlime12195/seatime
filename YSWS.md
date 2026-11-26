# Connecting an application to SeaTime

SeaTime counts hours with a one-page rule ([docs/RULE.md](docs/RULE.md)). Hand edits count the way that Hackatime counts them. The time of an AI agent depends on the server's AI check. While the check is off, the agent's time counts only in the 10 minutes after a hand edit or a new prompt. While the check is on, the agent's time does not count by itself. A prompt counts only if the check passes it, for up to 10 minutes. It counts for up to 15 when the person decides how part of the project should work. These two numbers are a server setting ([docs/RULE.md](docs/RULE.md#when-the-ai-check-is-on)). The API calls the counted number "struggle" (`struggle_seconds`, `struggle_hours`).

An application, such as the site of a Hack Club YSWS program, reads people's hours from SeaTime the way that it reads them from Hackatime. Nobody submits anything. Each computer sends the records of a project once a stretch of work on it is over. The server checks them against the hashes that the computer sent while the person worked. The hours that an application reads are the hours that the server counted from records that passed that check.

Replace `https://seatime.example.com` below with your SeaTime server's address.

## 1. Switch from Hackatime by changing the base URL

If your site calls Hackatime like this:

```
GET https://hackatime.hackclub.com/api/v1/users/{slack_id}/stats?features=projects&start_date=2025-06-01&end_date=2025-08-31
```

point it at SeaTime instead:

```
GET https://seatime.example.com/api/v1/users/{slack_id}/stats?features=projects&start_date=2025-06-01&end_date=2025-08-31
```

The JSON has the same shape. Public profiles need no token. To read a private profile, or to use `current`, send `Authorization: Bearer <access token>` with a token from section 3.

What is different:

- `total_seconds`, and every `hours` or `text` made from it, is checked time only: time that the server counted from records that matched the hash chain. Each object also has `struggle_seconds` and `presence_seconds`, which are the same number. `dark_flow_seconds` is the agent's time that did not count.
- Each project has `status`, `status_reason`, `as_of` and `pending_seconds` (section 5). Each answer has `as_of`: the time on the server when it answered.
- `X-Seatime-Metric` says what `total_seconds` is.

**Hackatime's trust colour.** The operator of the server can turn this lookup on (`SEATIME_HACKATIME_TRUST=1`). Then `trust_factor` is the colour of the account on Hackatime, looked up by its Slack ID. So a program that rejects `red` keeps doing so through SeaTime. Otherwise, it is `blue` for everyone. Each `trust_factor` object has `trust_source`: `hackatime` (Hackatime answered in the last 15 minutes), `stale` (Hackatime did not answer; this is its last answer, at most 7 days old), `not_on_hackatime`, `unavailable` (Hackatime did not answer and there is no earlier answer), `no_slack_id`, or `off`. In every case except `hackatime` and `stale`, the colour is `blue`. That is Hackatime's word for "not scored", and it is a value that a Hackatime client can parse. If your program must not pay an account whose colour is unknown, check `trust_source` too.

## 2. Register your application

Sign in to SeaTime and open `/developers`. Give the application a name and one to five redirect addresses. People see the name when they approve the application. Each address must start with `https://`, or with `http://localhost` or `http://127.0.0.1` while you test.

You get a client ID and a client secret. The page shows the secret once, because SeaTime keeps only its hash. If you lose it, make a new one on the same page. The old one stops working at once. When you delete the application, it loses access to the hours of everyone who approved it.

## 3. Ask for permission (OAuth 2.0)

SeaTime uses the authorization code flow with PKCE. PKCE is required, and the method must be `S256`. There is one scope, `read`: the person's project names, hours and commit list. An application never sees projects that the person hid, and never sees prompts or code.

**Send the person to the consent page:**

```
GET https://seatime.example.com/oauth/authorize
  ?response_type=code
  &client_id=stc_...
  &redirect_uri=https://yourapp.example/seatime/callback
  &state=<random, checked by you>
  &code_challenge=<BASE64URL(SHA256(code_verifier))>
  &code_challenge_method=S256
  &scope=read
```

The person signs in if they need to and sees: "<your application> will be able to see your project names, hours, and commit list." Below that is a table of their projects with the hours, commit counts and status that your application would read. They approve once. After that, your application gets a code without asking the person again, until the person revokes it in Settings.

The redirect address must match a registered one exactly, character for character. If it does not, or if the client ID is unknown, SeaTime shows an error and does not redirect the person. For any other problem (no `state`, no PKCE, a method other than `S256`, another scope), SeaTime sends them back to your address with `error` and your `state`. If the person cancels, you get `error=access_denied`.

**Trade the code for tokens** within 60 seconds. A code works only once.

```
POST https://seatime.example.com/oauth/token
Authorization: Basic base64(client_id:client_secret)
Content-Type: application/x-www-form-urlencoded

grant_type=authorization_code&code=...&redirect_uri=<the same address>&code_verifier=...
```

`client_id` and `client_secret` may go in the form instead. The answer:

```json
{"access_token": "sto_...", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "str_...", "scope": "read"}
```

An access token lasts an hour. A refresh token lasts 30 days and works only once: `grant_type=refresh_token&refresh_token=...` returns a new pair. If a code or a refresh token is used a second time, SeaTime treats it as stolen. It then revokes every token that came from the same approval. Your application then has to ask the person again.

`POST /oauth/revoke` with `token=...` (and your client credentials) revokes a token (RFC 7009). Errors follow RFC 6749: `invalid_client` with status 401, `invalid_grant` and the others with status 400.

## 4. Read hours

Every endpoint reads a public profile without a token. It reads any profile with a token that the owner of the profile approved. `{user}` is a Slack member ID (`U0123ABCD`), a Hack Club Auth ID (`ident!...`), a SeaTime username with or without `@`, or `current` (the token's user).

| Endpoint | Notes |
|---|---|
| `GET /api/v1/users/{user}/stats` (`features`, `start_date`, `end_date`, `filter_by_project`, `total_seconds=true`, `limit`) | Hackatime's shape. `projects[]` carry the status fields. `languages` is always `[]`. Dates accept `YYYY-MM-DD` or RFC3339; date-only values are UTC. |
| `GET /api/v1/users/{user}/projects` | Project names; the last 30 days by default, as on Hackatime. |
| `GET /api/v1/users/{user}/project/{name}` | `total_heartbeats` is the count of checked minutes. `archived` is always false. |
| `GET /api/v1/users/{user}/projects/details?projects=a,b` | Sorted by hours. |
| `GET /api/v1/users/{user}/projects/{project}/stats?since=&until=` | One project: checked seconds, status fields and `commit_list` (`sha`, `ts`, `ins`, `del`). `{project}` is the name, slug or key. A project is a folder on the user's computer: it stays one project, with the same slug, when its repo gets a remote, is renamed or moves to an org, and `project` in the answer is the key it goes by now. With the user's token, also `models` (see "Time by model" below). |
| `GET /api/v1/users/{user}/projects/{project}/ysws` and `/ysws.txt` | The export for the Unified YSWS database (section 6). |
| `GET /api/v1/users/{user}/trust_factor` | Hackatime's colour (section 1). |
| `GET /api/v1/users/lookup_slack_uid/{id}` | The SeaTime username as `user_id`. Public profiles only. |
| `GET /api/summary` (`user`/`user_id`, `interval`/`range`, `from`/`to`) | `projects[{key,total}]` with the status fields. |
| `GET /api/hackatime/v1/users/{id}` | |
| `GET /api/hackatime/v1/users/{id}/summaries` (`start`, `end`, `timezone`, `project`) | Daily `grand_total` and `projects[]` (each with `status`). At most 366 days. |
| `GET /api/hackatime/v1/users/{id}/statusbar/today` | `grand_total` only. Today's work is usually still pending (section 5). |
| `GET /api/hackatime/v1/users/{id}/stats/last_7_days` | `total_seconds`, `projects`, one `coding` category. |

SeaTime does not have Hackatime's admin API, email lookup, heartbeat spans, leaderboards, badges or heartbeat uploads. As on Hackatime, an unknown user gets 404 with `{"error": "User not found"}`, and a private profile gets 403. Answers without a token may be cached for 10 seconds. Answers with a token are not cached.

## 5. Status and as_of

Each project in an answer has:

- `status`: `verified`, `pending` or `failed`.
- `status_reason`: why, when the status is not `verified`.
- `as_of`: the time up to which the hours of the project are checked (the end of the last stretch that the server checked).
- `pending_seconds`: synced time after `as_of` that is not checked yet. It is not in `total_seconds`.

A project is **pending** when:

- recent work is not checked yet. Once an hour, on the hour, the server checks the records that the person's computer has sent. So the work of the last hour is usually pending;
- prompts are waiting for the AI check;
- the server asked the person's computer for prompts from a stretch when the AI check was off (below).

A project is **failed** when some of its records did not match the computer's hash chain (edited, backdated or forged records). It is also failed when prompts that the check needed never arrived. Those records do not count, and `status_reason` says what happened. Time from records that did match still counts.

**The AI check for stretches when it was off.** A person can turn the AI check off for their account. The first time that an application reads a project with prompts from such a stretch, SeaTime asks the person's computer for the saved prompts and checks them. Until that is done, the project is pending. If the prompts have not arrived after 14 days, they count as not passing, and the project is failed. Nothing changes for prompts that were checked when they were sent.

### Time by model

`GET /api/v1/users/{user}/projects/{project}/stats?since=&until=` also answers with `models` when the request carries the user's own token: the access token of an application that the user approved, or one of the user's device tokens. `since` and `until` are RFC3339 times. A request with no token, or with another user's token, gets the same answer without `models`. A hidden project answers 404 to an application, as everywhere else.

```json
{
  "user": "mo", "project": "github.com/mo/app", "status": "verified", "as_of": "2026-11-24T17:02:11Z",
  "human_seconds": 5400, "struggle_seconds": 5400, "dark_flow_seconds": 600, "pending_seconds": 0, "commits": 4,
  "models": [
    {"kind": "model", "model": "claude-opus-5-20260115", "seconds": 3000, "prompts": 21},
    {"kind": "model", "model": "gpt-5.2-codex", "seconds": 900, "prompts": 6},
    {"kind": "hand", "seconds": 1200, "prompts": 0},
    {"kind": "not_recorded", "seconds": 300, "prompts": 2}
  ]
}
```

- The rows' `seconds` add up to `human_seconds`: the checked time in the range.
- `kind` is `model`, `hand` or `not_recorded`. A `model` row names the model in `model`, exactly as the AI tool reported it. Rows of this kind are sorted by name.
- `hand` is time from hand edits. It has no model.
- `not_recorded` is AI time whose records name no model (Gemini CLI does not report one). It also holds all time that the server checked before it kept models: for that time the server stored neither the model nor the split between hand and AI.
- `prompts` counts every prompt in the checked records of the range, by the model that answered it. Prompts from records checked before the server kept models are not counted.
- A prompt's time goes to the model that answered it ([docs/RULE.md](docs/RULE.md), "Time by model").

## 6. The Unified YSWS export

```
GET /api/v1/users/{user}/projects/{project}/ysws       JSON
GET /api/v1/users/{user}/projects/{project}/ysws.txt   the copy-paste block
```

Fields:

- `hours`: the checked hours of the whole project, for your hours field.
- `status`, `status_reason`, `as_of` and `pending_hours`, as in section 5.
- `dark_flow_hours`: the agent's time that did not count.
- `override_hours_justification`: a short sentence that SeaTime writes, for a field that explains override hours.
- `commits`, `commit_range` (`first`, `last`, `count`, `compare_url`), `code_url` (the GitHub repo, when known), `from`, `to`.
- `share_url` (the project's public page) and `export_url`.
- `text`: the block below.

```
Override Hours Spent: 21.2 (SeaTime hours, verified)
Override Hours Spent Justification: 21.2h counted by SeaTime as Hackatime counts heartbeats; 17.3h of AI time not counted (more than 10 min after the person's last heartbeat); 14 commits; every record matched the device's hash chain; project: https://seatime.example.com/@someone/rocket
Code URL: https://github.com/someone/rocket
Commits: 1a2b3c4..9f8e7d6 (14) https://github.com/someone/rocket/compare/...
SeaTime project: https://seatime.example.com/@someone/rocket
SeaTime JSON: https://seatime.example.com/api/v1/users/someone/projects/rocket/ysws
```

For the Unified YSWS Projects DB ("Approved Projects" table), put `hours` in **Override Hours Spent** and `override_hours_justification` in **Override Hours Spent Justification**. Put `code_url` in the code URL field. Wait for `verified`. A pending number can still grow, and a failed one leaves out records that did not match.

## 7. What applications never see

Applications never see prompt text, shell commands or folder paths. A computer sends those fields in each record only as keyed hashes. The hash chain needs them, but nobody can read them back. The server keeps them to check the chain and never shows them. A project that is not a public repo goes by its folder's name until its owner gives it another. [pkg/api/WIRE.md](pkg/api/WIRE.md) lists every field that leaves a person's computer.

## Where the Hackatime shapes come from

The shapes above were taken from the public `hackclub/hackatime` source (`config/routes.rb`, `Api::V1::StatsController`, `Api::SummaryController`, `Api::Hackatime::V1::HackatimeController`, `WakatimeService`, `ProjectStatsQuery`) and from how open-source YSWS sites call it: `hackclub/shipwrecked`, `hackclub/athena-award`, `hackclub/milkyway`, `hackclub/summer-of-making` and `hackclub/stasis`.

## For participants

1. Install SeaTime with the line on the home page of your SeaTime server. When the browser opens, sign in with Hack Club, so that applications can find you by Slack ID.
2. Build your project as usual. Your hours show on your profile.
3. Push your code to a public GitHub repo, so that your commits can be checked.
4. An application, such as the site of your YSWS program, may ask to connect to SeaTime. Approve it. It then reads your project names, hours and commits by itself. You can revoke it in Settings at any time.

Your editor time also still reaches Hackatime, because SeaTime passes the editor plugin's heartbeats on unchanged.
