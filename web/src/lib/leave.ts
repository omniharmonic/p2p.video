// Leaving a call as a guest [SL-P1-6]: pure state machine and copy, so the
// rules are unit-tested without a browser. The component (LeaveCall.ts) owns
// the DOM; main.ts owns media teardown and rejoin.

/** What this device still has to send to the host. */
export interface UploadBacklog {
  /** A take is arming, recording or stopping on this device. */
  recording: boolean;
  /** Bytes journaled locally that the host has not acknowledged yet. */
  pendingBytes: number;
  /** Recorded tracks that have no host assembly receipt yet. */
  unfinishedTracks: number;
  /** Recent upload throughput, for an ETA. 0 or undefined when unknown. */
  rateBps?: number;
  /** Bytes acknowledged by the host in this page's lifetime. */
  uploadedBytes?: number;
}

export type LeaveState = 'in-call' | 'confirming' | 'left' | 'rejoining' | 'ended';
export type LeaveEvent =
  | { type: 'leave'; pending: boolean }
  | { type: 'confirm' }
  | { type: 'cancel' }
  | { type: 'rejoin' }
  | { type: 'admitted' }
  | { type: 'rejoin-failed' }
  | { type: 'meeting-ended' };

/** True while leaving would pause something the host still needs. */
export function uploadsPending(b: UploadBacklog): boolean {
  return b.recording || b.pendingBytes > 0 || b.unfinishedTracks > 0;
}

/**
 * Transitions. Leaving with pending uploads asks first; without, it leaves
 * at once. Rejoin goes back through admission (a resume grant admits without
 * a second host prompt). Anything unexpected keeps the current state.
 */
export function nextLeaveState(state: LeaveState, event: LeaveEvent): LeaveState {
  switch (event.type) {
    case 'leave': return state === 'in-call' ? (event.pending ? 'confirming' : 'left') : state;
    case 'confirm': return state === 'confirming' ? 'left' : state;
    case 'cancel': return state === 'confirming' ? 'in-call' : state;
    case 'rejoin': return state === 'left' ? 'rejoining' : state;
    case 'admitted': return state === 'rejoining' ? 'in-call' : state;
    case 'rejoin-failed': return state === 'rejoining' ? 'left' : state;
    case 'meeting-ended': return state === 'in-call' ? state : 'ended';
  }
}

export interface ConfirmCopy { title: string; body: string; confirmLabel: string }

/** The confirmation shown before leaving with pending uploads, or null. */
export function leaveConfirmation(b: UploadBacklog): ConfirmCopy | null {
  if (!uploadsPending(b)) return null;
  const body = b.recording
    ? 'A take is recording. Leaving stops your recording now. What is already saved keeps uploading while this tab stays open; if you close it, reopen this link from this browser to finish.'
    : 'Your recording is still uploading — leaving now pauses it; reopen this link from this browser to finish. Keep this tab open and it carries on after you leave.';
  return { title: 'Leave the call?', body, confirmLabel: 'Leave call' };
}

const MB = 1024 * 1024;

/** One line for the post-leave screen. */
export function leftUploadText(b: UploadBacklog): string {
  if (b.recording) return 'Finishing your recording…';
  if (b.pendingBytes > 0 || b.unfinishedTracks > 0) {
    const mb = Math.max(0.1, b.pendingBytes / MB);
    const eta = b.rateBps && b.rateBps > 0 && b.pendingBytes > 0 ? Math.ceil((b.pendingBytes * 8) / b.rateBps) : null;
    const etaText = eta === null ? '' : eta < 60 ? ' · less than a minute left' : ` · ~${Math.ceil(eta / 60)} min left`;
    return b.pendingBytes > 0
      ? `Uploading ${mb.toFixed(1)} MB${etaText}. Keep this tab open.`
      : 'Waiting for the host to confirm your recording. Keep this tab open.';
  }
  return (b.uploadedBytes ?? 0) > 0
    ? 'Your recording reached the host. You can close this tab.'
    : 'No recording on this device.';
}

/** Upload progress as 0–1, or null when there is nothing to show. */
export function leftUploadFraction(b: UploadBacklog): number | null {
  const done = b.uploadedBytes ?? 0;
  const total = done + Math.max(0, b.pendingBytes);
  if (total <= 0) return null;
  return Math.min(1, done / total);
}
