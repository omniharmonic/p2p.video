/**
 * Signaling reconnect delay [SL-P0-7].
 *
 * Media comes back only after the WebSocket does, so the time from "daemon
 * healthy again" to "call media back" is bounded by this delay. A service
 * restart (launchd throttles respawns to >=10 s) used to grow the old 15 s cap
 * and leave participants waiting up to 15 s after the host was back. The cap
 * is low because a failed attempt against loopback/tailnet/Funnel is cheap and
 * a session has a handful of participants. Jitter spreads simultaneous
 * reconnects of every participant after a restart.
 */
export const SIGNALING_RETRY_BASE_MS = 250;
export const SIGNALING_RETRY_MAX_MS = 3_000;
/** ±20 % jitter, so the worst case is SIGNALING_RETRY_MAX_MS × 1.2. */
export const SIGNALING_RETRY_JITTER = 0.2;

export function signalingRetryDelayMs(attempt: number, random: () => number = Math.random): number {
  const n = Number.isFinite(attempt) ? Math.max(0, Math.floor(attempt)) : 0;
  const base = Math.min(SIGNALING_RETRY_MAX_MS, SIGNALING_RETRY_BASE_MS * 2 ** Math.min(n, 16));
  const r = Math.min(1, Math.max(0, random()));
  return Math.round(base * (1 - SIGNALING_RETRY_JITTER + 2 * SIGNALING_RETRY_JITTER * r));
}
