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
 *   tool_execution_start    -> status "running" + start poll
 *     (while a tool runs, every 1.5s: if a <sid>.waiting flag is present
 *      -> "waiting"; else isIdle() ? "waiting" : "running". The flag is set
 *      by cooperating prompting extensions so the user-input window is
 *      visible even though isIdle() stays false while a tool awaits
 *      ctx.ui.confirm/input/select.)
 *   rpiv:ask-user:blocked (pi.events, rpiv-ask-user-question >= 2.6.2):
 *      subscription below writes "waiting" instantly; askUserBlocked gates
 *      the poll so a 1.5s tick can't clobber it.
 *
 * Cooperation protocol (for prompting extensions that do NOT emit events,
 * e.g. pi-claude-permissions or the bundled question.ts example):
 *   const sid = ctx.sessionManager.getSessionId();
 *   const sessionFile = ctx.sessionManager.getSessionFile();
 *   const liveDir = join(sessionFile, "..", "..", "..", "live");
 *   const flag = join(liveDir, `${sid}.waiting`);
 *   // before:
 *   writeFileSync(flag, "");
 *   const answer = await ctx.ui.confirm("Title", "Allow?");
 *   // after:
 *   try { unlinkSync(flag); } catch {}
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

const DEBUG = process.env.AGENT_SESSIONS_PI_LIVE_DEBUG === "1";
let lastWrittenStatus: LiveStatus | undefined;

function debug(ctx: ExtensionContext | undefined, message: string): void {
	if (!DEBUG) return;
	// eslint-disable-next-line no-console
	console.log(`[agent-sessions-pi-live] ${message}`);
	if (ctx?.hasUI) {
		ctx.ui.notify(`[pi-live] ${message}`, "info");
	}
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
		if (DEBUG && status !== lastWrittenStatus) {
			debug(ctx, `marker status: ${status}`);
			lastWrittenStatus = status;
		}
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

// During tool execution we poll every 1.5s to distinguish a tool that's
// actively working from one that's blocked on user input. isIdle() alone is
// not enough: it stays false while a custom tool awaits ctx.ui.confirm/
// input/select (verified: isIdle()=false during the ask-user-questions
// prompt). Two mechanisms force "waiting": (1) rpiv's rpiv:ask-user:blocked
// event sets askUserBlocked (checked first below), and (2) any prompting
// extension can create a sidecar
// flag file <liveDir>/<sid>.waiting before showing a prompt, and delete it
// after the user responds. When the flag is present, the poll forces
// "waiting" regardless of isIdle(). When it's absent, we fall back to
// isIdle() (true -> "waiting" for the tool's own setup gap; false ->
// "running"). On the last tool_execution_end we unlink the flag as
// defensive cleanup in case the cooperating extension crashed.
let toolCount = 0;
let pollDelay: ReturnType<typeof setTimeout> | undefined;
let pollTimer: ReturnType<typeof setInterval> | undefined;
let pollCtx: ExtensionContext | undefined;
// Most recent ExtensionContext, cached so the pi.events handler (whose
// payload carries no ctx/sid) can write markers for the current session.
let currentCtx: ExtensionContext | undefined;
// True while a cooperating extension reports a live ask-user prompt
// (rpiv:ask-user:blocked {active:true}). Gates the poll as described above.
let askUserBlocked = false;

function waitingFlagPath(ctx: ExtensionContext): string | undefined {
	const sid = ctx.sessionManager.getSessionId();
	if (!sid) return undefined;
	return join(liveDir(ctx), `${sid}.waiting`);
}

function startPoll(ctx: ExtensionContext): void {
	stopPoll();
	pollCtx = ctx;
	debug(ctx, "startPoll (tool_execution_start)");
	// Small delay before the first poll so a tool that's about to start
	// working doesn't briefly read isIdle()=true during its async setup.
	pollDelay = setTimeout(() => {
		pollDelay = undefined;
		if (toolCount === 0 || !pollCtx) return;
		debug(pollCtx, "poll: first tick");
		pollTimer = setInterval(() => {
			if (toolCount === 0 || !pollCtx) return;
			try {
				if (askUserBlocked) {
					debug(pollCtx, "poll: ask-user blocked -> waiting");
					writeMarker(pollCtx, "waiting");
					return;
				}
				const flag = waitingFlagPath(pollCtx);
				if (flag && existsSync(flag)) {
					debug(pollCtx, "poll: waiting flag present -> waiting");
					writeMarker(pollCtx, "waiting");
				} else {
					const idle = pollCtx.isIdle();
					debug(pollCtx, `poll: isIdle()=${idle}`);
					writeMarker(pollCtx, idle ? "waiting" : "running");
				}
			} catch (err) {
				debug(pollCtx, `poll error: ${(err as Error).message}`);
				stopPoll();
			}
		}, 1500);
	}, 800);
}

function stopPoll(): void {
	if (pollDelay) {
		clearTimeout(pollDelay);
		pollDelay = undefined;
	}
	if (pollTimer) {
		clearInterval(pollTimer);
		pollTimer = undefined;
	}
	pollCtx = undefined;
}

export default function (pi: ExtensionAPI) {
	pi.on("session_start", async (_event, ctx) => {
		currentCtx = ctx;
		writeMarker(ctx, "idle");
	});

	pi.on("agent_start", async (_event, ctx) => {
		currentCtx = ctx;
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
		currentCtx = ctx;
		if (event.message?.role !== "assistant") return;
		debug(ctx, "message_end (assistant) -> waiting");
		writeMarker(ctx, "waiting");
	});

	pi.on("tool_execution_start", async (event, ctx) => {
		currentCtx = ctx;
		toolCount++;
		debug(ctx, `tool_execution_start id=${event.toolCallId} name=${event.toolName} count=${toolCount}`);
		if (toolCount === 1) {
			writeMarker(ctx, "running");
			startPoll(ctx);
		}
	});

	pi.on("tool_execution_end", async (event, ctx) => {
		currentCtx = ctx;
		debug(ctx, `tool_execution_end id=${event.toolCallId} name=${event.toolName} isError=${event.isError} count=${toolCount}`);
		toolCount = Math.max(0, toolCount - 1);
		if (toolCount === 0) {
			stopPoll();
			// Defensive: a cooperating extension may have left the flag behind
			// or crashed without clearing askUserBlocked. Reset both so a
			// future tool run doesn't inherit a stale "waiting".
			askUserBlocked = false;
			const flag = waitingFlagPath(ctx);
			if (flag) {
				try { unlinkSync(flag); } catch {}
			}
		}
	});

	pi.on("agent_end", async (_event, ctx) => {
		currentCtx = ctx;
		writeMarker(ctx, "idle");
	});

	pi.on("session_shutdown", async (_event, ctx) => {
		currentCtx = ctx;
		stopPoll();
		removeMarker(ctx);
	});

	// rpiv-ask-user-question >= 2.6.2 emits rpiv:ask-user:blocked on the
	// shared pi.events bus when its questionnaire is awaiting the user
	// (and again with active:false in a finally when the wait ends). The
	// payload carries no ctx/sid, so markers use the cached currentCtx.
	pi.events.on("rpiv:ask-user:blocked", (payload: { active: boolean }) => {
		askUserBlocked = payload.active;
		if (!currentCtx) return;
		if (payload.active) {
			debug(currentCtx, "rpiv:ask-user:blocked -> waiting");
			writeMarker(currentCtx, "waiting");
		} else if (toolCount > 0) {
			// Answer received while a tool is still running: resume poll logic.
			writeMarker(currentCtx, currentCtx.isIdle() ? "waiting" : "running");
		}
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

	pi.registerCommand("pi-live-debug", {
		description: "Show agent-sessions-pi-live internal state",
		handler: async (_args, ctx) => {
			const path = markerPath(ctx);
			const present = path ? existsSync(path) : false;
			const idle = (() => {
				try { return ctx.isIdle(); } catch { return "ctx-stale"; }
			})();
			const flag = waitingFlagPath(ctx);
			const flagPresent = flag ? existsSync(flag) : false;
			const msg = `toolCount=${toolCount} pollActive=${pollTimer !== undefined} askUserBlocked=${askUserBlocked} isIdle()=${idle} waitingFlag=${flagPresent} marker=${present ? "present" : "missing"} lastWritten=${lastWrittenStatus ?? "none"}`;
			if (ctx.hasUI) {
				ctx.ui.notify(msg, "info");
			}
		},
	});
}
