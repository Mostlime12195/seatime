package store

// migrations are applied in order. Never edit one that has shipped: add a
// new one at the end. They are written for SQLite; pgMigration (dialect.go)
// translates them for Postgres.
var migrations = []string{
	`
CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  display_name  TEXT NOT NULL DEFAULT '',
  avatar        TEXT NOT NULL DEFAULT '',
  slack_id      TEXT NOT NULL DEFAULT '',
  github        TEXT NOT NULL DEFAULT '',
  public        INTEGER NOT NULL DEFAULT 1,
  role          TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('user','admin')),
  tz_offset_min INTEGER NOT NULL DEFAULT 0,
  created       INTEGER NOT NULL,
  -- start of the user's newest bucket, for "who is active now"
  last_bucket   INTEGER NOT NULL DEFAULT 0,
  -- a session cookie carries this; bumping it (logout, a role change)
  -- ends every session of the user
  session_epoch INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX users_slack ON users(slack_id) WHERE slack_id <> '';
CREATE INDEX users_last_bucket ON users(last_bucket);

CREATE TABLE identities (
  provider TEXT NOT NULL,
  subject  TEXT NOT NULL,
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (provider, subject)
) WITHOUT ROWID;
CREATE INDEX identities_user ON identities(user_id);

CREATE TABLE devices (
  id             TEXT PRIMARY KEY,
  user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name           TEXT NOT NULL,
  os             TEXT NOT NULL DEFAULT '',
  pubkey         TEXT NOT NULL,
  token_hash     TEXT NOT NULL UNIQUE,
  last_seen      INTEGER NOT NULL DEFAULT 0,
  created        INTEGER NOT NULL,
  revoked        INTEGER NOT NULL DEFAULT 0,
  -- the SeaTime version the device last synced with
  client_version TEXT NOT NULL DEFAULT ''
);
CREATE INDEX devices_user ON devices(user_id);

CREATE TABLE device_codes (
  code_hash   TEXT PRIMARY KEY,
  user_code   TEXT NOT NULL UNIQUE,
  device_name TEXT NOT NULL,
  os          TEXT NOT NULL DEFAULT '',
  pubkey      TEXT NOT NULL,
  expires     INTEGER NOT NULL,
  user_id     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX device_codes_expires ON device_codes(expires);

CREATE TABLE projects (
  id      INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key     TEXT NOT NULL,
  slug    TEXT NOT NULL,
  name    TEXT NOT NULL DEFAULT '',
  remote  TEXT NOT NULL DEFAULT '',
  repo    TEXT NOT NULL DEFAULT '',
  hidden  INTEGER NOT NULL DEFAULT 0,
  created INTEGER NOT NULL,
  -- the owner has chosen whether the project is public
  decided INTEGER NOT NULL DEFAULT 0,
  UNIQUE (user_id, key),
  UNIQUE (user_id, slug)
);

-- One row per user, project, active minute and device. start_min is unix
-- seconds / 60. Readers merge devices per minute: MAX for durations (a
-- minute can't hold more than 60 s of one person), SUM for counts.
CREATE TABLE buckets (
  user_id    INTEGER NOT NULL,
  project_id INTEGER NOT NULL,
  start_min  INTEGER NOT NULL,
  device_id  TEXT NOT NULL DEFAULT '',
  human      REAL NOT NULL,
  struggle   REAL NOT NULL,
  dark_flow  REAL NOT NULL,
  agent      REAL NOT NULL DEFAULT 0,
  prompts    INTEGER NOT NULL DEFAULT 0,
  edits      INTEGER NOT NULL DEFAULT 0,
  commits    INTEGER NOT NULL DEFAULT 0,
  runs       INTEGER NOT NULL DEFAULT 0,
  harnesses  TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (user_id, project_id, start_min, device_id)
) WITHOUT ROWID;

-- Hourly totals, kept up to date by UpsertBuckets. hour is unix seconds / 3600.
CREATE TABLE hourly (
  user_id    INTEGER NOT NULL,
  project_id INTEGER NOT NULL,
  hour       INTEGER NOT NULL,
  human      REAL NOT NULL DEFAULT 0,
  struggle   REAL NOT NULL DEFAULT 0,
  dark_flow  REAL NOT NULL DEFAULT 0,
  prompts    INTEGER NOT NULL DEFAULT 0,
  edits      INTEGER NOT NULL DEFAULT 0,
  commits    INTEGER NOT NULL DEFAULT 0,
  runs       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, project_id, hour)
) WITHOUT ROWID;
CREATE INDEX hourly_user_hour ON hourly(user_id, hour);
CREATE INDEX hourly_hour ON hourly(hour, user_id, struggle, dark_flow);

CREATE TABLE kv (name TEXT PRIMARY KEY, value REAL NOT NULL) WITHOUT ROWID;

-- append-only
CREATE TABLE checkpoints (
  device_id TEXT NOT NULL,
  seq       INTEGER NOT NULL,
  hash      TEXT NOT NULL,
  server_ts INTEGER NOT NULL,
  PRIMARY KEY (device_id, seq)
) WITHOUT ROWID;
CREATE INDEX checkpoints_hash ON checkpoints(hash);

CREATE TABLE commits (
  project_id INTEGER NOT NULL,
  sha        TEXT NOT NULL,
  ts         INTEGER NOT NULL,
  ins        INTEGER NOT NULL DEFAULT 0,
  del        INTEGER NOT NULL DEFAULT 0,
  verified   INTEGER NOT NULL DEFAULT 0,
  message    TEXT NOT NULL DEFAULT '',
  author     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (project_id, sha)
) WITHOUT ROWID;
CREATE INDEX commits_ts ON commits(project_id, ts);
CREATE INDEX commits_unchecked ON commits(project_id) WHERE verified = 0;

CREATE TABLE submissions (
  id         TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id INTEGER NOT NULL,
  device_id  TEXT NOT NULL,
  verified   INTEGER NOT NULL,
  problems   TEXT NOT NULL DEFAULT '[]',
  claimed    TEXT NOT NULL,
  scored     TEXT NOT NULL,
  seq_lo     INTEGER NOT NULL,
  seq_hi     INTEGER NOT NULL,
  score_ver  TEXT NOT NULL DEFAULT '',
  created    INTEGER NOT NULL
);
CREATE INDEX submissions_user ON submissions(user_id, project_id, created);
CREATE INDEX submissions_created ON submissions(created);
CREATE TABLE submission_blobs (id TEXT PRIMARY KEY, data BLOB NOT NULL);

-- Net credited human seconds per minute of each verified submission, so
-- the same minute is not credited twice:
CREATE TABLE submission_minutes (
  submission_id TEXT NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
  user_id       INTEGER NOT NULL,
  start_min     INTEGER NOT NULL,
  human         REAL NOT NULL,
  PRIMARY KEY (submission_id, start_min)
) WITHOUT ROWID;
CREATE INDEX submission_minutes_user ON submission_minutes(user_id, start_min);
-- Hackatime's trust level for a Slack ID (trust.go); found=0: no such
-- Hackatime account. The count never reads it.
CREATE TABLE hackatime_trust (
  slack_id TEXT PRIMARY KEY,
  level    TEXT NOT NULL,
  value    INTEGER NOT NULL,
  found    INTEGER NOT NULL,
  checked  INTEGER NOT NULL
) WITHOUT ROWID;
-- The prompt check (pkg/promptcheck). Per prompt: the model's yes/no
-- answers (JSON; a question not asked is null, and listed in skipped), the
-- decision, the kind, the model, the minutes given, the rules used, and
-- what the prompt was judged on, as numbers and hashes. Keyed by device and
-- the prompt's salted hash. Never any text.
CREATE TABLE prompt_verdicts (
  device_id  TEXT NOT NULL,
  check_hash TEXT NOT NULL,
  labels     TEXT NOT NULL,
  pass       INTEGER NOT NULL,
  kind       TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL,
  prompt_ts  INTEGER NOT NULL DEFAULT 0,
  created    INTEGER NOT NULL,
  minutes    REAL NOT NULL DEFAULT 0,
  rules      TEXT NOT NULL DEFAULT '',
  skipped    TEXT NOT NULL DEFAULT '',
  evidence   TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (device_id, check_hash)
) WITHOUT ROWID;
CREATE INDEX prompt_verdicts_ts ON prompt_verdicts(device_id, prompt_ts);
CREATE INDEX prompt_verdicts_rules ON prompt_verdicts(rules);
-- Time waiting for the prompt check, as each device last reported it, by
-- project key (seconds of the last 24 hours).
CREATE TABLE pending_time (
  device_id   TEXT NOT NULL,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_key TEXT NOT NULL,
  seconds     REAL NOT NULL,
  at          INTEGER NOT NULL,
  PRIMARY KEY (device_id, project_key)
) WITHOUT ROWID;
CREATE INDEX pending_time_user ON pending_time(user_id);
-- Paid check calls per UTC day and account, for the daily limits.
CREATE TABLE check_spend (
  day     TEXT NOT NULL,
  user_id INTEGER NOT NULL,
  calls   INTEGER NOT NULL DEFAULT 0,
  fails   INTEGER NOT NULL DEFAULT 0,
  cost    REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (day, user_id)
) WITHOUT ROWID;
-- When the prompt check was on or off: from each from_ms on, on_ says
-- which. A record is counted with the rule of its own time. Rows are only
-- ever added.
CREATE TABLE rule_periods (
  from_ms INTEGER PRIMARY KEY,
  on_     INTEGER NOT NULL,
  by      TEXT NOT NULL DEFAULT ''
);

-- Settings an admin changes on /admin. A key with no row takes its default
-- from the environment. settings_log keeps every change and who made it.
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated    INTEGER NOT NULL,
  updated_by TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE settings_log (
  id  INTEGER PRIMARY KEY,
  key TEXT NOT NULL,
  old TEXT,
  new TEXT,
  by  TEXT NOT NULL,
  at  INTEGER NOT NULL
);
`,
	// 2: tables earlier versions wrote and this one no longer reads.
	`
DROP TABLE IF EXISTS reviews;
DROP TABLE IF EXISTS submission_prompts;
DROP TABLE IF EXISTS nda_overrides;
DROP TABLE IF EXISTS gh_reachable;
DROP TABLE IF EXISTS gh_repos;
DROP TABLE IF EXISTS sync_addrs;
DROP TABLE IF EXISTS rhythm_days;
DROP TABLE IF EXISTS rhythm_done;
`,
	// 3: the prompt chain, each account's own prompt check setting, and the
	// server asking devices for saved prompts (promptchain.go)
	`
-- Each device's prompt chain (pkg/promptcheck chain.go): one row per
-- prompt the device recorded, with the check on or off. Hashes, times and
-- project keys only, never text. status is '' while open, 'mismatch' when
-- the text sent for it did not match, 'missing' when it was asked for and
-- never came before its deadline.
CREATE TABLE prompt_chain (
  device_id   TEXT NOT NULL,
  seq         INTEGER NOT NULL,
  ts          INTEGER NOT NULL,
  project_key TEXT NOT NULL,
  commit_hash TEXT NOT NULL,
  rep         INTEGER NOT NULL DEFAULT 0,
  link        TEXT NOT NULL,
  received    INTEGER NOT NULL,
  status      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (device_id, seq)
) WITHOUT ROWID;
CREATE INDEX prompt_chain_commit ON prompt_chain(device_id, commit_hash);
CREATE INDEX prompt_chain_ts ON prompt_chain(device_id, ts);
-- An account's own prompt check setting: from from_ms on, on_ says which.
-- With no rows it is on. Rows are only ever added.
CREATE TABLE user_check_periods (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  from_ms INTEGER NOT NULL,
  on_     INTEGER NOT NULL,
  PRIMARY KEY (user_id, from_ms)
) WITHOUT ROWID;
-- The server asking an account's devices for their saved prompts sent in
-- [from_ms, to_ms), of one project key or of every project (''). Devices
-- send them for their check on their next sync, whatever the account's
-- setting. What has not come by the deadline is marked missing.
CREATE TABLE prompt_pulls (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_key TEXT NOT NULL DEFAULT '',
  from_ms     INTEGER NOT NULL,
  to_ms       INTEGER NOT NULL,
  created     INTEGER NOT NULL,
  deadline    INTEGER NOT NULL,
  by          TEXT NOT NULL DEFAULT '',
  closed      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX prompt_pulls_user ON prompt_pulls(user_id, closed);
`,
	// 4: settings an account sets on the web for its computers, each
	// computer's last check-in, and requests the web queues for a computer
	// (devicereq.go); a sign-in that reconnects an earlier device; a project
	// name chosen on the web
	`
-- An account's settings for its computers (api.Setting*): value is 0 or 1.
CREATE TABLE account_settings (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key     TEXT NOT NULL,
  value   INTEGER NOT NULL,
  updated INTEGER NOT NULL,
  PRIMARY KEY (user_id, key)
) WITHOUT ROWID;
-- Each device's last check-in (api.CheckIn as JSON: version, what it can
-- do, its AI tools, problem codes, its settings). No paths, no text.
CREATE TABLE device_health (
  device_id TEXT PRIMARY KEY,
  report    TEXT NOT NULL,
  at        INTEGER NOT NULL
) WITHOUT ROWID;
-- What the owner asked a device to do on the web (api.Request*). status:
-- queued, sent (handed to the device at a check-in), done, failed.
CREATE TABLE device_requests (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_id   TEXT NOT NULL,
  kind        TEXT NOT NULL,
  project_key TEXT NOT NULL DEFAULT '',
  purge       INTEGER NOT NULL DEFAULT 0,
  status      TEXT NOT NULL DEFAULT 'queued',
  error       TEXT NOT NULL DEFAULT '',
  count       INTEGER NOT NULL DEFAULT 0,
  created     INTEGER NOT NULL,
  updated     INTEGER NOT NULL
);
CREATE INDEX device_requests_device ON device_requests(device_id, status);
CREATE INDEX device_requests_user ON device_requests(user_id, created);
-- a sign-in from a device the server stopped accepting, signed with its
-- key: approving it reconnects that device
ALTER TABLE device_codes ADD COLUMN prev_device TEXT NOT NULL DEFAULT '';
-- the owner named the project on the web: a name a device sends no longer replaces it
ALTER TABLE projects ADD COLUMN named INTEGER NOT NULL DEFAULT 0;
`,
	// 5: no submissions: devices upload their records as they work and the
	// server checks them (segments.go); applications read hours over OAuth
	// (oauth.go)
	`
DROP TABLE IF EXISTS submission_minutes;
DROP TABLE IF EXISTS submission_blobs;
DROP TABLE IF EXISTS submissions;
-- One stretch of one project's records from one device, checked against
-- the device's hash chain when it arrives and again when prompt checks
-- come back. status: verified, pending (prompts wait for their check) or
-- failed (problems says why). Times are unix ms.
CREATE TABLE segments (
  id         TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id INTEGER NOT NULL,
  device_id  TEXT NOT NULL,
  status     TEXT NOT NULL,
  problems   TEXT NOT NULL DEFAULT '[]',
  claimed    TEXT NOT NULL,
  scored     TEXT NOT NULL,
  seq_lo     INTEGER NOT NULL,
  seq_hi     INTEGER NOT NULL,
  score_ver  TEXT NOT NULL DEFAULT '',
  first_ts   INTEGER NOT NULL DEFAULT 0,
  last_ts    INTEGER NOT NULL DEFAULT 0,
  pending    INTEGER NOT NULL DEFAULT 0,
  created    INTEGER NOT NULL,
  checked    INTEGER NOT NULL
);
CREATE INDEX segments_project ON segments(user_id, project_id, created);
CREATE INDEX segments_status ON segments(status);
CREATE TABLE segment_blobs (id TEXT PRIMARY KEY, data BLOB NOT NULL);
-- Credited seconds per minute of each segment that did not fail, after
-- every deduction: what applications read, and what keeps a minute from
-- counting twice. dark is the AI time not counted.
CREATE TABLE segment_minutes (
  segment_id TEXT NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL,
  project_id INTEGER NOT NULL,
  start_min  INTEGER NOT NULL,
  human      REAL NOT NULL,
  dark       REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (segment_id, start_min)
) WITHOUT ROWID;
CREATE INDEX segment_minutes_user ON segment_minutes(user_id, start_min);
CREATE INDEX segment_minutes_project ON segment_minutes(project_id, start_min);
-- an application read the project, so its prompts were asked for (ShipPull)
ALTER TABLE projects ADD COLUMN pulled INTEGER NOT NULL DEFAULT 0;

-- OAuth 2.0 (authorization code with PKCE). Secrets, codes and tokens are
-- stored as SHA-256 hashes only.
CREATE TABLE oauth_clients (
  id            TEXT PRIMARY KEY,
  user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  redirect_uris TEXT NOT NULL,
  secret_hash   TEXT NOT NULL,
  created       INTEGER NOT NULL
);
CREATE INDEX oauth_clients_user ON oauth_clients(user_id);
-- A user approved an application once; revoking deletes the row and its tokens.
CREATE TABLE oauth_grants (
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  client_id TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
  scope     TEXT NOT NULL,
  created   INTEGER NOT NULL,
  PRIMARY KEY (user_id, client_id)
) WITHOUT ROWID;
CREATE TABLE oauth_codes (
  code_hash    TEXT PRIMARY KEY,
  client_id    TEXT NOT NULL,
  user_id      INTEGER NOT NULL,
  redirect_uri TEXT NOT NULL,
  challenge    TEXT NOT NULL,
  scope        TEXT NOT NULL,
  expires      INTEGER NOT NULL,
  used         INTEGER NOT NULL DEFAULT 0,
  family       TEXT NOT NULL
) WITHOUT ROWID;
-- family ties every token issued from one code together: reusing a code
-- or a rotated refresh token revokes the whole family.
CREATE TABLE oauth_tokens (
  token_hash TEXT PRIMARY KEY,
  kind       TEXT NOT NULL,
  client_id  TEXT NOT NULL,
  user_id    INTEGER NOT NULL,
  scope      TEXT NOT NULL,
  family     TEXT NOT NULL,
  expires    INTEGER NOT NULL,
  revoked    INTEGER NOT NULL DEFAULT 0,
  created    INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX oauth_tokens_grant ON oauth_tokens(user_id, client_id);
CREATE INDEX oauth_tokens_family ON oauth_tokens(family);
`,
	// 6: an account deleted by its owner (deleteaccount.go): unix time of
	// the deletion, 0 for a live account. The row stays, with nothing in it,
	// only until its computers have removed SeaTime.
	`
ALTER TABLE users ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0;
`,
	// 7: counted time and prompts by AI model (models.go). Time synced
	// before this has no model on record: its hand seconds go in as hand
	// time and its AI seconds and prompts as "no model" (model ''), so that
	// every hour's rows add up to hourly.struggle from the start.
	`
ALTER TABLE buckets ADD COLUMN models TEXT NOT NULL DEFAULT '';
-- Per user, project and hour: counted seconds and prompts by model. hand=1
-- (model '') is hand time; hand=0 with model '' is AI time and prompts
-- whose records name no model.
CREATE TABLE hourly_models (
  user_id    INTEGER NOT NULL,
  project_id INTEGER NOT NULL,
  hour       INTEGER NOT NULL,
  hand       INTEGER NOT NULL DEFAULT 0,
  model      TEXT NOT NULL DEFAULT '',
  seconds    REAL NOT NULL DEFAULT 0,
  prompts    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, project_id, hour, hand, model)
) WITHOUT ROWID;
CREATE INDEX hourly_models_user_hour ON hourly_models(user_id, hour);
-- a minute's seconds are those of the device that counted most (as hourly)
INSERT INTO hourly_models(user_id,project_id,hour,hand,model,seconds,prompts)
SELECT user_id, project_id, start_min/60, 1, '', SUM(struggle - ai), 0 FROM (
  SELECT user_id, project_id, start_min, struggle,
    CASE WHEN agent < 0 THEN 0 WHEN agent > struggle THEN struggle ELSE agent END AS ai,
    ROW_NUMBER() OVER (PARTITION BY user_id, project_id, start_min ORDER BY struggle DESC, device_id) AS rn
  FROM buckets) t
WHERE rn = 1 AND struggle - ai > 0 GROUP BY user_id, project_id, start_min/60;
INSERT INTO hourly_models(user_id,project_id,hour,hand,model,seconds,prompts)
SELECT user_id, project_id, start_min/60, 0, '', SUM(ai), 0 FROM (
  SELECT user_id, project_id, start_min, struggle,
    CASE WHEN agent < 0 THEN 0 WHEN agent > struggle THEN struggle ELSE agent END AS ai,
    ROW_NUMBER() OVER (PARTITION BY user_id, project_id, start_min ORDER BY struggle DESC, device_id) AS rn
  FROM buckets) t
WHERE rn = 1 AND ai > 0 GROUP BY user_id, project_id, start_min/60;
INSERT INTO hourly_models(user_id,project_id,hour,hand,model,seconds,prompts)
SELECT user_id, project_id, start_min/60, 0, '', 0, SUM(prompts) FROM buckets
GROUP BY user_id, project_id, start_min/60 HAVING SUM(prompts) > 0
ON CONFLICT(user_id,project_id,hour,hand,model) DO UPDATE SET prompts=excluded.prompts;
-- The same split of each segment's credited minutes: what applications
-- read. Per segment and minute the seconds add up to segment_minutes.human.
CREATE TABLE segment_models (
  segment_id TEXT NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL,
  project_id INTEGER NOT NULL,
  start_min  INTEGER NOT NULL,
  hand       INTEGER NOT NULL DEFAULT 0,
  model      TEXT NOT NULL DEFAULT '',
  seconds    REAL NOT NULL DEFAULT 0,
  prompts    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (segment_id, start_min, hand, model)
) WITHOUT ROWID;
CREATE INDEX segment_models_project ON segment_models(project_id, start_min);
CREATE INDEX segment_models_user ON segment_models(user_id);
`,
	// 8: a project is a folder, and its key no longer changes with its git
	// remote (projectkeys.go). project_keys holds every key each device has
	// sent and the project it belongs to, for good; label is what the
	// device's folder goes by now, and projects.key what the project goes
	// by now. Keys sent so far belong to the project they made. After this,
	// foldProjects folds the projects an older device split (migrate).
	`
CREATE TABLE project_keys (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_id  TEXT NOT NULL,
  key        TEXT NOT NULL,
  project_id INTEGER NOT NULL,
  label      TEXT NOT NULL,
  created    INTEGER NOT NULL,
  PRIMARY KEY (user_id, device_id, key)
) WITHOUT ROWID;
CREATE INDEX project_keys_project ON project_keys(project_id);
INSERT INTO project_keys(user_id,device_id,key,project_id,label,created)
SELECT DISTINCT b.user_id, b.device_id, p.key, p.id, p.key, p.created FROM buckets b JOIN projects p ON p.id=b.project_id;
INSERT INTO project_keys(user_id,device_id,key,project_id,label,created)
SELECT DISTINCT s.user_id, s.device_id, p.key, p.id, p.key, p.created FROM segments s JOIN projects p ON p.id=s.project_id
WHERE true ON CONFLICT(user_id,device_id,key) DO NOTHING;
INSERT INTO project_keys(user_id,device_id,key,project_id,label,created)
SELECT DISTINCT d.user_id, c.device_id, p.key, p.id, p.key, p.created FROM prompt_chain c JOIN devices d ON d.id=c.device_id
JOIN projects p ON p.user_id=d.user_id AND p.key=c.project_key
WHERE true ON CONFLICT(user_id,device_id,key) DO NOTHING;
-- A name that still leads to a project that no longer goes by it: a key it
-- went by before, or the slug of a project that was folded into it.
CREATE TABLE project_links (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  project_id INTEGER NOT NULL,
  PRIMARY KEY (user_id, name)
) WITHOUT ROWID;
`,
	// 9: a device sends its records as it writes them (records.go), and the
	// server cuts and counts each project's stretches itself, on its own
	// clock. A segment the server made (stream=1) covers its project's
	// records with a time in [since_ts, until_ts); the last one of a device
	// and project is open (is_open=1, no until_ts): it is counted again at
	// each cycle until its stretch of work is over.
	`
-- A device's chain as it sent it: every record in its private form, or
-- as a bare leaf hash (data ''). Kept only once it replayed to a
-- checkpoint, so it is the chain the checkpoints commit to. prev is the
-- chain head before the record.
CREATE TABLE records (
  device_id TEXT NOT NULL,
  seq       INTEGER NOT NULL,
  ts        INTEGER NOT NULL DEFAULT 0,
  project   TEXT NOT NULL DEFAULT '',
  kind      TEXT NOT NULL DEFAULT '',
  leaf      TEXT NOT NULL,
  prev      TEXT NOT NULL,
  data      TEXT NOT NULL DEFAULT '',
  path      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (device_id, seq)
) WITHOUT ROWID;
CREATE INDEX records_ts ON records(device_id, ts);
CREATE INDEX records_project ON records(device_id, project, ts);
-- How far each device's records go: head is the last seq held, run_lo the
-- first seq of the unbroken run that ends there (a device that lost its
-- log goes on after a hole). dirty is the earliest time among the records
-- that came since the server last counted (0: none came).
CREATE TABLE record_streams (
  device_id TEXT PRIMARY KEY,
  user_id   INTEGER NOT NULL,
  head      INTEGER NOT NULL,
  run_lo    INTEGER NOT NULL,
  dirty     INTEGER NOT NULL DEFAULT 0,
  received  INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX record_streams_dirty ON record_streams(dirty);
ALTER TABLE segments ADD COLUMN stream INTEGER NOT NULL DEFAULT 0;
ALTER TABLE segments ADD COLUMN is_open INTEGER NOT NULL DEFAULT 0;
ALTER TABLE segments ADD COLUMN since_ts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE segments ADD COLUMN until_ts INTEGER NOT NULL DEFAULT 0;
-- the project key its records carry (the project may go by another now)
ALTER TABLE segments ADD COLUMN rec_key TEXT NOT NULL DEFAULT '';
-- the server counts on a cycle (internal/server records.go): when the
-- last one was claimed, and the mark of the last one that finished
INSERT INTO kv(name,value) VALUES ('count_claim',0);
INSERT INTO kv(name,value) VALUES ('count_done',0);
CREATE INDEX segments_stream ON segments(stream, is_open);
`,
}
