#!/bin/sh
# macOS smoke test: build, show the menu bar icon for a moment, install into
# a throwaway home, check the launchd agent loads, record one fake hook event, read it back, uninstall, and check
# nothing is left. Used by .github/workflows/macos.yml (manual dispatch) and
# runnable on any Mac:  sh scripts/mac-smoke.sh
# With no Go on the Mac, point it at a built binary instead:
#   SEATIME_BIN=~/Downloads/seatime-darwin-arm64 sh scripts/mac-smoke.sh
# It never touches your real home: everything lives under a temp directory.
# It takes about a minute and ends with "mac smoke: PASS" or the failing step.
set -eu
[ "$(uname -s)" = Darwin ] || { echo "macOS only"; exit 1; }
T=$(mktemp -d -t seatime-smoke)
cleanup() {
  launchctl bootout "gui/$(id -u)/dev.hackclub.seatime" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT INT TERM
fail() { echo "FAIL: $*"; exit 1; }
step() { printf '\n== %s\n' "$*"; }

step build
if [ -n "${SEATIME_BIN:-}" ]; then
  cp "$SEATIME_BIN" "$T/seatime" && chmod 755 "$T/seatime"
  xattr -d com.apple.quarantine "$T/seatime" 2>/dev/null || true
elif command -v go >/dev/null 2>&1; then
  go build -o "$T/seatime" ./cmd/seatime
else
  fail "no Go here: install it (brew install go) or set SEATIME_BIN to a built seatime binary"
fi
"$T/seatime" version 2>/dev/null || true

step "menu bar icon"
# the icon and its menu come up for a moment (needs a build with cgo, on a Mac's own desktop session)
"$T/seatime" tray --check | tee "$T/tray.txt"
grep -qx 'tray ok' "$T/tray.txt" || fail "this build cannot show the menu bar icon"
export SEATIME_FAKE_HOME="$T/home" SEATIME_HOME="$T/home/Library/Application Support/seatime" SEATIME_SERVER="http://127.0.0.1:9"
mkdir -p "$SEATIME_FAKE_HOME/.claude"
# a pre-existing user setting that must survive byte for byte
printf '{\n  "model": "opus"\n}\n' > "$SEATIME_FAKE_HOME/.claude/settings.json"
cp "$SEATIME_FAKE_HOME/.claude/settings.json" "$T/settings.orig"

step "setup (no login, no wakatime)"
start=$(date +%s)
"$T/seatime" setup --yes --no-login --no-wakatime </dev/null
plist="$SEATIME_FAKE_HOME/Library/LaunchAgents/dev.hackclub.seatime.plist"
[ -f "$plist" ] || fail "no LaunchAgent plist"
plutil -lint "$plist" || fail "plist does not lint"

step "agent loaded"
for i in 1 2 3 4 5 6 7 8 9 10; do
  if launchctl print "gui/$(id -u)/dev.hackclub.seatime" >/dev/null 2>&1; then break; fi
  sleep 1
done
launchctl print "gui/$(id -u)/dev.hackclub.seatime" | grep -E 'state|program' || fail "agent not loaded"

step "one hook event"
printf '{"session_id":"smoke","cwd":"%s","hook_event_name":"UserPromptSubmit","prompt":"hello"}' "$T" |
  "$T/seatime" hook claude-code UserPromptSubmit
"$T/seatime" status | tee "$T/status.txt"
grep -Eq 'events: +[2-9]' "$T/status.txt" || fail "hook event not recorded"
grep -q 'daemon: *running' "$T/status.txt" || fail "daemon not running under launchd"
echo "install to recording: $(( $(date +%s) - start )) s"

step "doctor --report"
"$T/seatime" doctor --report | tee "$T/report.txt"
grep -q 'agent loaded' "$T/report.txt" || fail "report does not see the agent"
grep -q '^summary:' "$T/report.txt" || fail "report has no summary line"
grep -q 'idle timer: *ioreg' "$T/report.txt" || echo "note: idle timer not readable on this runner"

step uninstall
"$T/seatime" uninstall </dev/null
if launchctl print "gui/$(id -u)/dev.hackclub.seatime" >/dev/null 2>&1; then fail "agent still loaded"; fi
[ ! -f "$plist" ] || fail "plist left behind"
cmp "$SEATIME_FAKE_HOME/.claude/settings.json" "$T/settings.orig" || fail "settings.json not restored byte for byte"
left=$(cd "$SEATIME_FAKE_HOME" && find . -type f ! -path './Library/Application Support/seatime/*' ! -path './.claude/settings.json')
[ -z "$left" ] || fail "left behind: $left"
echo; echo "mac smoke: PASS"
