// Pure helpers for collaboration state. Self-contained (no imports) so they
// can be unit-tested under node.

export interface HandEntry {
  participantId: string;
  name: string;
  handRaised: boolean;
  handRaisedAt?: number;
}

/** Raised hands in the order they went up (the host's speaking queue). */
export function handQueue<T extends HandEntry>(entries: readonly T[]): T[] {
  return entries
    .filter(e => e.handRaised)
    .sort((a, b) => (a.handRaisedAt ?? 0) - (b.handRaisedAt ?? 0) || a.participantId.localeCompare(b.participantId));
}

export function roleLabel(role: string): string {
  switch (role) {
    case 'host': return 'Host';
    case 'producer': return 'Producer';
    case 'subsource': return 'Extra source';
    default: return 'Guest';
  }
}

/** What a denied participant is told, by server reason code. */
export function denialMessage(reason: string | undefined): { status: string; detail: string } {
  switch (reason) {
    case 'removed':
      return { status: 'Removed from the call by the host', detail: 'The host removed you from this call. Your camera and microphone are no longer shared and recording on this device has stopped.' };
    case 'room_locked':
      return { status: 'This call is locked', detail: 'The host has locked this call, so new people can’t join right now. Ask the host to unlock it, then reopen your invitation.' };
    case 'busy':
      return { status: 'The host is busy right now', detail: 'Too many people are waiting to join, or join attempts are coming in too fast. Wait a minute, then reopen your invitation.' };
    case 'declined':
      return { status: 'The host declined your request', detail: 'The host declined your request to join. Reopen your invitation to ask again.' };
    default:
      return { status: 'Access expired or declined · reopen the invitation to request admission', detail: 'Admission declined or session resume expired. Reopen your invitation to request access.' };
  }
}

/** Short relative-free clock label for a chat timestamp (local time). */
export function chatTime(ts: number, locale?: string): string {
  return new Date(ts).toLocaleTimeString(locale, { hour: 'numeric', minute: '2-digit' });
}

/** Merge a replayed history with already-shown messages, oldest first, no duplicates. */
export function mergeChat<T extends { id: string; ts: number }>(shown: readonly T[], incoming: readonly T[], limit = 200): T[] {
  const byId = new Map<string, T>();
  for (const m of shown) byId.set(m.id, m);
  for (const m of incoming) byId.set(m.id, m);
  const seq = (id: string) => Number(id.replace(/\D/g, '')) || 0;
  return [...byId.values()].sort((a, b) => a.ts - b.ts || seq(a.id) - seq(b.id)).slice(-limit);
}

export interface WaitingEntry { participantId: string; status: string; role: string }

/**
 * Who the admission banner offers next, in the dashboard's arrival order. The
 * host alone can admit a producer [SL-P1-11], so a waiting producer comes
 * first for the host and the producer can then admit the guests; a producer
 * sees waiting guests only [SL-QA-1].
 */
export function nextAdmission<T extends WaitingEntry>(participants: readonly T[], viewer: 'host' | 'producer'): T | undefined {
  const waiting = participants.filter(p => p.status === 'pending' && (p.role === 'guest' || (viewer === 'host' && p.role === 'producer')));
  return waiting.find(p => p.role === 'producer') ?? waiting[0];
}
