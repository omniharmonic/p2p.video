// Phone as an extra camera (PRD F3) [SL-P1-9]: pure helpers shared by the
// participant's "Add phone camera" dialog and the phone page.
import type { SubsourceLinkResponse } from './protocol';

export interface PhoneLink { url: string; reachable: boolean; expiresAt: number }

const loopback = (host: string) => host === 'localhost' || host === '[::1]' || /^127\./.test(host);

/**
 * The URL to put in the QR code. The server sets baseUrl when the requester
 * is on the local dashboard (a phone can't open localhost); otherwise the
 * page's own origin (Serve/Funnel) is what the phone can reach too.
 */
export function phoneLink(resp: SubsourceLinkResponse, pageOrigin: string): PhoneLink {
  const base = (resp.baseUrl || pageOrigin).replace(/\/+$/, '');
  const url = new URL(resp.path, base + '/');
  return { url: url.toString(), reachable: !loopback(url.hostname), expiresAt: resp.expiresAt };
}

/** "Works once · expires in 9:41" (or that it has expired). */
export function expiryText(expiresAt: number, now: number): string {
  const left = Math.max(0, Math.ceil((expiresAt - now) / 1000));
  if (left === 0) return 'This code has expired. Make a new one.';
  return `Works once · expires in ${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}`;
}

export type PhoneState = 'starting' | 'connecting' | 'live' | 'countdown' | 'recording' | 'uploading' | 'done' | 'reconnecting' | 'ended' | 'error';

export interface PhoneStatus { state: PhoneState; uploadedBytes?: number; pendingBytes?: number; parentName?: string; message?: string }

/** The phone page's status line: one short, reassuring sentence. */
export function phoneStatusText(s: PhoneStatus): string {
  switch (s.state) {
    case 'starting': return 'Starting the camera…';
    case 'connecting': return 'Joining the call…';
    case 'live': return s.parentName ? `Live in the call as ${s.parentName}’s phone. Keep this page open.` : 'Live in the call. Keep this page open.';
    case 'countdown': return 'Recording starts…';
    case 'recording': return 'Recording on this phone. Keep this page open.';
    case 'uploading': {
      const total = (s.uploadedBytes ?? 0) + (s.pendingBytes ?? 0);
      const pct = total > 0 ? Math.floor(((s.uploadedBytes ?? 0) / total) * 100) : 0;
      return `Uploading ${pct}% — keep this page open.`;
    }
    case 'done': return 'Recording uploaded. You can keep filming or close this page.';
    case 'reconnecting': return 'Connection lost — reconnecting. Recording continues on this phone.';
    case 'ended': return s.message || 'This phone camera has left the call.';
    case 'error': return s.message || 'Something went wrong.';
  }
}

/** Phone camera constraints: full quality for the recording; the call sender is capped. */
export function phoneVideoConstraints(facing: 'user' | 'environment'): MediaTrackConstraints {
  return { facingMode: { ideal: facing }, width: { ideal: 1920 }, height: { ideal: 1080 }, frameRate: { ideal: 30 } };
}

/** The phone's room mic records raw, like every recording mic. */
export const PHONE_AUDIO_CONSTRAINTS: MediaTrackConstraints = { echoCancellation: false, noiseSuppression: false, autoGainControl: false };
