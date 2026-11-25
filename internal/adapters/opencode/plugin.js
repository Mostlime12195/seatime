// seatime-managed: installed by `seatime setup`; removed by `seatime uninstall`. Do not edit.
// Copyright (C) 2026 the SeaTime authors. Licensed under the GNU AGPL v3.
// SPDX-License-Identifier: AGPL-3.0-only
// Forwards opencode activity to `seatime hook opencode <event>` (JSON on stdin).
// Fire-and-forget: never blocks or throws into opencode.
import { spawn } from "node:child_process"

const SEATIME_BIN = __SEATIME_BIN__
const args = new Map() // callID -> tool args (the after-hook doesn't carry them)
let loaded = false

function send(ev, payload) {
  try {
    const p = spawn(SEATIME_BIN, ["hook", "opencode", ev], { stdio: ["pipe", "ignore", "ignore"], windowsHide: true, detached: true })
    p.on("error", () => {})
    p.stdin.on("error", () => {})
    p.stdin.end(JSON.stringify({ ts: new Date().toISOString(), ...payload }))
    p.unref()
  } catch (_) {}
}

function build(cwd) {
  if (loaded) return {} // both export styles may be probed by different opencode versions
  loaded = true
  const dir = () => cwd || process.cwd()
  return {
    event: async ({ event }) => {
      try {
        const props = (event && event.properties) || {}
        const sid = props.sessionID || (props.info && props.info.id) || ""
        if (event.type === "session.created") send("session_start", { session_id: sid, cwd: dir() })
        else if (event.type === "session.idle") send("agent_stop", { session_id: sid, cwd: dir() })
        else if (event.type === "session.deleted") send("session_end", { session_id: sid, cwd: dir() })
      } catch (_) {}
    },
    "chat.message": async (input, output) => {
      try {
        const parts = (output && output.parts) || []
        const text = parts.filter((x) => x && x.type === "text" && !x.synthetic).map((x) => x.text).join("\n")
        const m = (input && input.model) || {}
        send("prompt", { session_id: (input && input.sessionID) || "", cwd: dir(), text, model: m.modelID ? (m.providerID ? m.providerID + "/" : "") + m.modelID : "" })
      } catch (_) {}
    },
    "tool.execute.before": async (input, output) => {
      try {
        if (input && input.callID) {
          args.set(input.callID, output && output.args)
          if (args.size > 200) args.delete(args.keys().next().value)
        }
      } catch (_) {}
    },
    "tool.execute.after": async (input, output) => {
      try {
        const a = args.get(input.callID) || {}
        args.delete(input.callID)
        send("tool", { session_id: input.sessionID || "", cwd: dir(), tool: input.tool, args: a, metadata: (output && output.metadata) || {} })
      } catch (_) {}
    },
  }
}

// opencode >= plugin v1 API: default export with server()
export default { id: "seatime", server: async (input) => build(input && (input.directory || (input.location && input.location.directory))) }
// older opencode: every function export is a plugin
export const SeatimePlugin = async ({ directory }) => build(directory)
