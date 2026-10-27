// Host/producer source health [SL-TECH-1]: the admission banner and the
// session health aside, rendered from the hub's dashboard telemetry.
// Untrusted strings (names, track states, connection paths) are escaped or set
// with textContent [SL-SEC-1].
import { CallNetwork } from '../components/CallNetwork';
import { nextAdmission } from '../lib/callstate';
import { recordingMicLabel } from '../lib/micmode';
import { DashboardPayload, RoleProducer, TypeAdmit, TypeDeny } from '../lib/protocol';
import { rememberParticipantNames } from './call';
import {
  admissionGuestName, admitBtn, closeHealthBtn, denyBtn, healthParticipantsList, hostAdmissionBanner,
  sessionHealthAside, toggleHealthBtn,
} from './dom';
import { preflight } from './greenroom';
import { sendEnvelope } from './signaling';
import { canAssist, state } from './state';
import { showToast } from './ui';

/** Host admission banner: admit or decline the next waiting participant. */
export function bindAdmissionBanner(): void {
  if (admitBtn) {
    admitBtn.onclick = () => {
      if (state.pendingGuestId) {
        sendEnvelope({
          t: TypeAdmit,
          participantId: state.participantId,
          payload: { participantId: state.pendingGuestId },
        });
        showToast(`${admissionGuestName?.textContent || 'Guest'} joined`);
        if (hostAdmissionBanner) hostAdmissionBanner.style.display = 'none';
        state.pendingGuestId = null;
      }
    };
  }

  if (denyBtn) {
    denyBtn.onclick = () => {
      if (state.pendingGuestId) {
        sendEnvelope({
          t: TypeDeny,
          participantId: state.participantId,
          payload: { participantId: state.pendingGuestId, reason: 'Host declined admission' },
        });
        showToast(`${admissionGuestName?.textContent || 'Guest'} wasn’t let in`);
        if (hostAdmissionBanner) hostAdmissionBanner.style.display = 'none';
        state.pendingGuestId = null;
      }
    };
  }
}

/** Session health aside open/close. */
export function bindHealthAside(): void {
  if (toggleHealthBtn) {
    toggleHealthBtn.onclick = () => {
      if (sessionHealthAside) {
        sessionHealthAside.style.display = sessionHealthAside.style.display === 'none' ? 'flex' : 'none';
      }
    };
  }

  if (closeHealthBtn) {
    closeHealthBtn.onclick = () => {
      if (sessionHealthAside) {
        sessionHealthAside.style.display = 'none';
      }
    };
  }
}

export function renderDashboard(dash: DashboardPayload): void {
  rememberParticipantNames(dash.participants);
  if (canAssist()) preflight.setParticipants(dash.participants || [], state.participantId, dash.preflightRefreshAtMs);
  // 1. Admission banner: the host for waiting guests and producers, a
  // producer for waiting guests only (the hub enforces it) [SL-P1-11].
  const nextPending = nextAdmission(dash.participants || [], state.isHost ? 'host' : 'producer');
  state.latestDashboard = dash;
  if (canAssist() && nextPending) {
    state.pendingGuestId = nextPending.participantId;
    if (admissionGuestName) admissionGuestName.innerText = (nextPending.name || 'Guest Participant') + (nextPending.role === RoleProducer ? ' (producer)' : '');
    if (hostAdmissionBanner) hostAdmissionBanner.style.display = 'flex';
  } else {
    if (hostAdmissionBanner) hostAdmissionBanner.style.display = 'none';
    state.pendingGuestId = null;
  }

  // 2. Session Health Aside Monitor
  if (healthParticipantsList) {
    healthParticipantsList.innerHTML = '';
    (dash.participants || []).forEach((p) => {
      const card = document.createElement('div');
      card.className = 'health-source';
      card.dataset.status = String(p.status);
      const rttMs = Math.round(p.rttUs / 1000);
      const isYou = p.participantId === state.participantId;
      const status = p.status === 'admitted' ? 'In the call' : p.status === 'pending' ? 'Waiting to join' : 'Left';
      // Recording: what reached the host, in plain words [SL-UX-2].
      let recording = p.role === RoleProducer ? '' : 'Not recording';
      const tracks = p.tracks ? Object.values(p.tracks) : [];
      if (tracks.length) {
        const t = tracks[0];
        const mb = (t.uploadedBytes / (1024 * 1024)).toFixed(1);
        const st = String(t.state);
        recording = st === 'done' ? `Saved on the host (${mb} MB)` : st === 'error' ? `Upload problem, retrying (${mb} MB saved)` : st === 'recording' ? `Recording, ${mb} MB on the host` : `Uploading, ${mb} MB on the host`;
      }
      // Upload backlog + connection quality [SL-P0-8].
      const backlog = CallNetwork.participantBacklog(p);
      const connection = p.netStats ? `Connection ${p.netStats.quality}${p.netStats.path === 'relay' ? ', relayed' : ''}${rttMs ? `, ${rttMs} ms` : ''}` : rttMs ? `${rttMs} ms to the host` : '';
      // Whose recording mic is raw or clean [SL-P1-10]; producers aren't recorded [SL-P1-11].
      const micLabel = p.role === RoleProducer ? 'Producer · not recorded' : p.preflight?.recordingMic ? recordingMicLabel(p.preflight.recordingMic) : '';
      const lines = [connection, recording, backlog ?? ''].filter(Boolean).map(l => `<span>${escapeHTML(l)}</span>`).join('');
      card.innerHTML = `
        <div class="health-source-header">
          <strong>${escapeHTML(p.name || 'Participant')}${isYou ? ' (you)' : ''}</strong>
          <span class="health-status">${escapeHTML(status)}</span>
        </div>
        <div class="health-source-meta">${lines}${micLabel ? `<span class="health-recmic" data-recmic="${escapeHTML(p.preflight?.recordingMic?.resolved ?? '')}">${escapeHTML(micLabel)}</span>` : ''}</div>
      `;
      healthParticipantsList.appendChild(card);
    });
    const summary = CallNetwork.backlogSummary(dash);
    if (summary) {
      const warn = document.createElement('div');
      warn.className = 'field-hint health-upload-backlog';
      warn.textContent = summary;
      healthParticipantsList.prepend(warn);
    }
  }
}

/** HTML-escapes an untrusted string for the health cards' markup. */
export function escapeHTML(value: string): string {const e=document.createElement('span');e.textContent=value;return e.innerHTML;}
