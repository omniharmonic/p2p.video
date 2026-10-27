// Signaling socket lifecycle [SL-TECH-1]: connect, hello, clock pings,
// reconnect with backoff, and envelope dispatch through a handler table
// (registered by the composition root) instead of an if/else chain.
//
// Reconnect signaling without touching capture streams, OPFS, or upload queues.
// The signed in-memory participant grant restores admission without storing a
// biometric/media credential in browser storage or adding it to a URL.
import { signalingRetryDelayMs } from '../lib/backoff';
import { ClockSync } from '../lib/clock';
import { Envelope, HelloPayload, TypeClockPing, TypeHello } from '../lib/protocol';
import { PeerClosedError } from '../lib/rtc';
import { clearRemoteMedia } from './call';
import { nameInput, recBtn, roleSelect, telemetryStatus } from './dom';
import { state } from './state';
import { showToast, updateConnectionPill } from './ui';

export type EnvelopeHandler = (env: Envelope) => void | Promise<void>;

/** The current signaling socket (null before joining and after leaving). */
export let ws: WebSocket | null = null;
export const clockSync = new ClockSync();
let clockTimer: number | null = null;
let signalingChain = Promise.resolve();
let reconnectTimer: number | null = null;
let reconnectAttempt = 0;
let resumeBlocked = false;
let handlers: Partial<Record<string, EnvelopeHandler>> = {};
let fallback: EnvelopeHandler = () => {};

/**
 * Envelope types handled by the app, and the handler for everything else
 * (feature components that each ignore types they don't own).
 */
export function registerEnvelopeHandlers(table: Partial<Record<string, EnvelopeHandler>>, otherwise: EnvelopeHandler): void {
  handlers = table;
  fallback = otherwise;
}

async function handleSignalingMessage(env: Envelope): Promise<void> {
  const handler = handlers[env.t];
  await (handler ? handler(env) : fallback(env));
}

export function sendEnvelope(env: Envelope): void {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(env));
  }
}

export function signalingOpen(): boolean {
  return ws?.readyState === WebSocket.OPEN;
}

/** Runs `task` after any signaling work already queued on this socket. */
export function queueSignalingWork(task: () => Promise<void>, onError: () => void): void {
  signalingChain = signalingChain.then(task).catch(onError);
}

export function openSignalingConnection(): void {
 if (resumeBlocked || !state.hasJoined) return;
 if (reconnectTimer !== null) {clearTimeout(reconnectTimer);reconnectTimer=null;}
 const previous = ws;
 if(previous){previous.onopen=previous.onmessage=previous.onclose=previous.onerror=null;previous.close();}
 if(clockTimer){clearInterval(clockTimer);clockTimer=null;}
 clearRemoteMedia();
 if(!state.uploadToken)state.participantId='';
 const proto=location.protocol==='https:'?'wss:':'ws:';
 const socket=new WebSocket(`${proto}//${location.host}/api/v1/ws?session=${encodeURIComponent(state.sessionId)}&token=${encodeURIComponent(state.token)}`);
 ws=socket;
 // Each socket gets a fresh message chain: work left over from the previous
 // socket (e.g. an SDP step on a closed peer) must never block this one [SL-P0-7].
 signalingChain=Promise.resolve();
 socket.onopen=()=>{
  if(ws!==socket)return;
  updateConnectionPill();
  const hello:HelloPayload={name:nameInput.value.trim()||'Participant',role:roleSelect.value as 'host'|'guest',resumeToken:state.uploadToken||undefined,capabilities:{webcodecs:false,opfs:!!navigator.storage?.getDirectory}};
  sendEnvelope({t:TypeHello,payload:hello});
  let lastMessage=performance.now();
  socket.addEventListener('message',()=>{lastMessage=performance.now();});
  clockTimer=window.setInterval(()=>{
   if(performance.now()-lastMessage>10000){openSignalingConnection();return;}
   sendEnvelope({t:TypeClockPing,payload:clockSync.createPing()});
  },1000);
  sendEnvelope({t:TypeClockPing,payload:clockSync.createPing()});
 };
 socket.onmessage=e=>{signalingChain=signalingChain.then(()=>{if(ws===socket)return handleSignalingMessage(JSON.parse(e.data));}).catch(err=>{if(!(err instanceof PeerClosedError))showToast('Connection message failed. Reconnecting keeps saved media.');});};
 socket.onerror=()=>{if(ws===socket)telemetryStatus.textContent='Connection unavailable · retrying automatically';};
 socket.onclose=()=>{
  if(ws!==socket)return;
  if(clockTimer){clearInterval(clockTimer);clockTimer=null;}
  clearRemoteMedia();recBtn.disabled=true;
  if(resumeBlocked)return;
  const delay=signalingRetryDelayMs(reconnectAttempt++);
  telemetryStatus.textContent=state.isRecording?'Connection lost · recording locally · reconnecting…':'Connection lost · reconnecting…';
  reconnectTimer=window.setTimeout(openSignalingConnection,delay);
 };
}

/** Reconnects when the tab becomes visible or the network returns; stops on unload. */
export function bindSignalingLifecycle(): void {
  document.addEventListener('visibilitychange',()=>{if(document.visibilityState==='visible' && state.hasJoined && ws?.readyState!==WebSocket.OPEN)openSignalingConnection();});
  window.addEventListener('online',()=>{if(state.hasJoined && ws?.readyState!==WebSocket.OPEN)openSignalingConnection();});
  window.addEventListener('beforeunload',()=>{resumeBlocked=true;if(reconnectTimer!==null)clearTimeout(reconnectTimer);});
}

/** Admission succeeded: the next disconnect retries from the shortest delay. */
export function resetReconnectBackoff(): void {
  reconnectAttempt = 0;
}

/** Denied or ended by the host: never reconnect from this page. */
export function blockReconnect(): void {
  resumeBlocked=true; if(reconnectTimer!==null)clearTimeout(reconnectTimer);
}

/** The host ended the meeting: close the socket (its close handler stays). */
export function closeSignaling(): void {
  ws?.close();
}

/** Leave call [SL-P1-6]: stop reconnecting and drop the socket without close handling. */
export function leaveSignaling(): void {
  resumeBlocked = true;
  if (reconnectTimer !== null) { clearTimeout(reconnectTimer); reconnectTimer = null; }
}

export function detachSignaling(): void {
  const socket = ws; ws = null;
  if (socket) { socket.onopen = socket.onmessage = socket.onclose = socket.onerror = null; socket.close(); }
  if (clockTimer) { clearInterval(clockTimer); clockTimer = null; }
}

/** Rejoin after leaving: reconnect with the in-memory resume grant. */
export function rejoinSignaling(): void {
  resumeBlocked = false; reconnectAttempt = 0;
  openSignalingConnection();
}
