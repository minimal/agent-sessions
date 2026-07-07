/**
 * agent-sessions-pi-live
 *
 * A pi extension that writes a small JSON marker file for the current session
 * whenever pi's lifecycle changes. agent-sessions reads these markers to show
 * live state (running / waiting / idle), the process pid, and the tmux pane
 * hosting the session.
 *
 * The marker is written to:
 *   ~/.pi/agent/live/<session-uuid>.json
 *
 * Lifecycle:
 *   session_start  -> status "idle"
 *   agent_start    -> status "running"
 *   message_end (assistant) -> status "waiting"
 *     (the LLM just stopped; if a tool is about to run, tool_execution_start
 *      flips it back to "running")
 *   tool_execution_start    -> status "running" + start isIdle() poll
 *     (while a tool runs, every 1.5s: isIdle() ? "waiting" : "running", so a
 *      custom tool that awaits ctx.ui.confirm/input for the user shows as
 *      "waiting" instead of "running")
 *   tool_execution_end      -> stop poll
 *   agent_end      -> status "idle"
 *   session_shutdown -> stop poll, delete marker
 */

import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { existsSync, mkdirSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";

type LiveStatus = "idle" | "running" | "waiting";

interface LiveMarker {
	sid: string;
	pid: number;
	pane: string | null;
	cwd: string;
	status: LiveStatus;
	updated_at: string;
}

function liveDir(ctx: ExtensionContext): string {
	// getSessionDir() returns the per-project directory that holds the actual
	// .jsonl file (e.g. ~/.pi/agent/sessions/--path-encoded-cwd--). The global
	// session storage root is its grandparent: ~/.pi/agent/sessions. The marker
	// directory lives next to that root at ~/.pi/agent/live.
	const sessionFile = ctx.sessionManager.getSessionFile();
	if (sessionFile) {
		return join(sessionFile, "..", "..", "..", "live");
	}
	// Fallback for in-memory/ephemeral sessions: derive from getSessionDir().
	return join(ctx.sessionManager.getSessionDir(), "..", "..", "live");
}

function markerPath(ctx: ExtensionContext): string | undefined {
	const sid = ctx.sessionManager.getSessionId();
	if (!sid) return undefined;
	return join(liveDir(ctx), `${sid}.json`);
}

function writeMarker(ctx: ExtensionContext, status: LiveStatus): void {
	const sid = ctx.sessionManager.getSessionId();
	if (!sid) return;

	const dir = liveDir(ctx);
	try {
		mkdirSync(dir, { recursive: true });
	} catch {
		return;
	}

	const marker: LiveMarker = {
		sid,
		pid: process.pid,
		pane: process.env.TMUX_PANE ?? null,
		cwd: ctx.sessionManager.getCwd(),
		status,
		updated_at: new Date().toISOString(),
	};

	const path = join(dir, `${sid}.json`);
	const tmp = `${path}.tmp`;
	try {
		writeFileSync(tmp, JSON.stringify(marker, null, 2));
		renameSync(tmp, path);
	} catch {
		// Best-effort: never let a marker write crash pi.
	}
}

function removeMarker(ctx: ExtensionContext): void {
	const path = markerPath(ctx);
	if (!path) return;
	try {
		unlinkSync(path);
	} catch {
		// Already gone or unreadable; that's fine.
	}
}

// isIdle() distinguishes a tool that's actively working from one that's
// blocked on user input. When a custom tool (e.g. an ask-user-questions tool
// the LLM called) awaits ctx.ui.confirm/input/select, the LLM is no longer
// streaming, so isIdle() is true even though tool_execution_start has fired
// and tool_execution_end hasn't. We poll isIdle() during tool execution so
// permission prompts (handled by the gap before tool_execution_start) and
// in-tool user prompts both surface as "waiting".
let inTool = false;
let pollTimer: ReturnType<typeof setInterval> | undefined;
let pollCtx: ExtensionContext | undefined;

function startPoll(ctx: ExtensionContext): void {
	stopPoll();
	pollCtx = ctx;
	// Small delay before the first poll so a tool that's about to start
	// working doesn't briefly read isIdle()=true during its async setup.
	setTimeout(() => {
		if (!inTool || !pollCtx) return;
		pollTimer = setInterval(() => {
			if (!inTool || !pollCtx) return;
			try {
				writeMarker(pollCtx, pollCtx.isIdle() ? "waiting" : "running");
			} catch {
				// ctx became stale (session replaced/reloaded); stop polling.
				stopPoll();
			}
		}, 1500);
	}, 800);
}

function stopPoll(): void {
	if (pollTimer) {
		clearInterval(pollTimer);
		pollTimer = undefined;
	}
	pollCtx = undefined;
}

export default function (pi: ExtensionAPI) {
	pi.on("session_start", async (_event, ctx) => {
		writeMarker(ctx, "idle");
	});

	pi.on("agent_start", async (_event, ctx) => {
		writeMarker(ctx, "running");
	});

	// When the LLM stops streaming (assistant message_end), the agent enters
	// a non-streaming window. The next event distinguishes the two cases:
	//   - tool_execution_start fires -> the tool is actually running -> "running"
	//   - no tool fires (permission prompt, another extension's confirm/input,
	//     or the turn is ending) -> stays "waiting"
	// This is the closest the extension API gets to "blocked on the user":
	// pi's built-in permission prompt holds the tool back, so
	// tool_execution_start does NOT fire while the prompt is up, and the
	// marker correctly shows "waiting".
	pi.on("message_end", async (event, ctx) => {
		if (event.message?.role !== "assistant") return;
		writeMarker(ctx, "waiting");
	});

	pi.on("tool_execution_start", async (_event, ctx) => {
		inTool = true;
		writeMarker(ctx, "running");
		startPoll(ctx);
	});

	pi.on("tool_execution_end", async (_event, _ctx) => {
		inTool = false;
		stopPoll();
	});

	pi.on("agent_end", async (_event, ctx) => {
		writeMarker(ctx, "idle");
	});

	pi.on("session_shutdown", async (_event, ctx) => {
		stopPoll();
		removeMarker(ctx);
	});

	pi.registerCommand("pi-live-status", {
		description: "Show agent-sessions-pi-live marker info",
		handler: async (_args, ctx) => {
			const path = markerPath(ctx);
			const present = path ? existsSync(path) : false;
			const sid = ctx.sessionManager.getSessionId();
			const dir = liveDir(ctx);
			const msg = `sid=${sid ?? "(none)"} dir=${dir} marker=${present ? "present" : "missing"}`;
			if (ctx.hasUI) {
				ctx.ui.notify(msg, present ? "info" : "error");
			}
		},
	});
}
