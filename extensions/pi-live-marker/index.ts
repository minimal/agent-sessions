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
 *   agent_end      -> status "idle"
 *   session_shutdown -> delete marker
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

function log(ctx: ExtensionContext, message: string): void {
	if (!DEBUG) return;
	// eslint-disable-next-line no-console
	console.log(`[agent-sessions-pi-live] ${message}`);
	if (ctx.hasUI) {
		ctx.ui.notify(`[pi-live] ${message}`, "info");
	}
}

function liveDir(ctx: ExtensionContext): string {
	const sessionDir = ctx.sessionManager.getSessionDir();
	return join(sessionDir, "..", "live");
}

function markerPath(ctx: ExtensionContext): string | undefined {
	const sid = ctx.sessionManager.getSessionId();
	if (!sid) return undefined;
	return join(liveDir(ctx), `${sid}.json`);
}

function writeMarker(ctx: ExtensionContext, status: LiveStatus): void {
	const sid = ctx.sessionManager.getSessionId();
	if (!sid) {
		log(ctx, "writeMarker: no session id");
		return;
	}

	const dir = liveDir(ctx);
	try {
		mkdirSync(dir, { recursive: true });
	} catch (err) {
		log(ctx, `mkdir failed: ${(err as Error).message}`);
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
		log(ctx, `wrote ${path} status=${status}`);
	} catch (err) {
		log(ctx, `write failed: ${(err as Error).message}`);
	}
}

function removeMarker(ctx: ExtensionContext): void {
	const path = markerPath(ctx);
	if (!path) {
		log(ctx, "removeMarker: no session id");
		return;
	}
	try {
		unlinkSync(path);
		log(ctx, `removed ${path}`);
	} catch {
		// Already gone or unreadable; that's fine.
	}
}

export default function (pi: ExtensionAPI) {
	if (DEBUG) {
		// eslint-disable-next-line no-console
		console.log("[agent-sessions-pi-live] extension loaded");
	}

	pi.on("session_start", async (event, ctx) => {
		log(ctx, `session_start reason=${event.reason}`);
		writeMarker(ctx, "idle");
	});

	pi.on("agent_start", async (_event, ctx) => {
		log(ctx, "agent_start");
		writeMarker(ctx, "running");
	});

	pi.on("agent_end", async (_event, ctx) => {
		log(ctx, "agent_end");
		writeMarker(ctx, "idle");
	});

	pi.on("session_shutdown", async (event, ctx) => {
		log(ctx, `session_shutdown reason=${event.reason}`);
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
			// eslint-disable-next-line no-console
			console.log(`[agent-sessions-pi-live] ${msg}`);
		},
	});
}
