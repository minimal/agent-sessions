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
import { mkdirSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
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

export default function (pi: ExtensionAPI) {
	pi.on("session_start", async (_event, ctx) => {
		writeMarker(ctx, "idle");
	});

	pi.on("agent_start", async (_event, ctx) => {
		writeMarker(ctx, "running");
	});

	pi.on("agent_end", async (_event, ctx) => {
		writeMarker(ctx, "idle");
	});

	pi.on("session_shutdown", async (_event, ctx) => {
		removeMarker(ctx);
	});
}
