// p2p.video web client entry [SL-TECH-1]: composes the app modules in
// web/src/app/ (module map: docs/ARCHITECTURE.md §4.6). This file owns only
// the order things are created and mounted, where cross-module controls land
// in the page, and the signaling dispatch table.
//
// Stylesheet cascade (docs/design/design-system.md): the CSS files and the
// components that import CSS are listed in their historical order, so the
// bundled stylesheet is identical whichever app module uses a component.
// Design system first so feature styles layer on top of it [SL-UX-1].
import { annotateSourceLinks } from './lib/brandmark';
import { entryMode } from './lib/rooms';
import './tokens.css';
import './studio.css';
import './preflight.css';
import './components/LayoutSwitcher';
import './components/LivestreamPanel';
import './components/OnboardingWizard';
import './components/OPFSRecoveryBanner';
import './components/TransfersRecoveryModal';
import './components/EcosystemActions';
import './components/SettingsDrawer';
import './components/Preflight';
import './components/CallFeatures';
import './components/RecordingMic';
import './components/ProducerLink';
import './components/ScreenShare';
import './components/DevicePicker';
import './components/CallNetwork';
import './call-features.css';
import './views.css';
import './components/Home';
import './home.css';
import './components/Captions';
import './captions.css';
import './components/CallChrome';
import './components/LeaveCall';
import './components/NoiseSuppression';
import './components/BackgroundEffects';
import './components/PhoneCamera';
import './components/Teleprompter';
import './components/Branding';
import './call.css';
import './components/InviteDialog';
import { mountCallChrome } from './components/CallChrome';
import {
  type AdmissionPayload, type ClockPongPayload, type DashboardPayload, type IceCandidatePayload,
  type ParticipantStatePayload, type PreflightPayload, type RecordStartPayload, type RoomStatePayload,
  type SdpPayload, TypeAdmission, TypeChirp, TypeClockPong, TypeDashboard, TypeIceCandidate, TypeParticipantState,
  TypePreflight, TypeRecordStart, TypeRecordStop, TypeRoomState, TypeSdpAnswer, TypeSdpOffer, TypeSessionEnd,
} from './lib/protocol';
import {
  createCallNetwork, createCallPeer, createLayoutSwitcher, createScreenShare, onIceCandidate, onParticipantState,
  onSdpAnswer, onSdpOffer, screenShare,
} from './app/call';
import { bindAdmissionBanner, bindHealthAside, renderDashboard } from './app/dashboard';
import { joinBtn } from './app/dom';
import {
  bindDeviceSelects, bindDiagnostics, bindJoin, createMeetingEntry, createOnboarding, createPreflight,
  initGreenRoom, onPreflightRequest,
} from './app/greenroom';
import {
  bindMuteControls, createDevicePicker, createEffects, createNoiseSuppression, createRecordingMic, devicePicker,
  effects, initDevices, noiseSuppression, recordingMic,
} from './app/media';
import {
  bindExportModal, bindTransfersButtons, createEcosystemActions, createRecoveryBanner, createTransfersModal,
  onRecordStart, stopLocalRecording,
} from './app/recording';
import { bindSignalingLifecycle, clockSync, registerEnvelopeHandlers } from './app/signaling';
import {
  bindInvite, bindStudioControls, callChromeOptions, createCallFeatures, createLeaveCall, createLivestream, createPhoneCamera,
  createProducerLink, createProductionExtras, createSettingsDrawer, dispatchToComponents, onAdmission, onChirp,
  onRoomState, onSessionEnd, phoneCamera,
} from './app/studio';
import { updateConnectionPill, updateResolutionSpec } from './app/ui';

// Creation order is load-bearing: components that add controls to shared
// containers (#more-list, the dock, the green room) do so as they mount.
createLivestream();
createEcosystemActions();
initGreenRoom();
createMeetingEntry();
createTransfersModal();
createSettingsDrawer();
createLayoutSwitcher();
createCallFeatures();
createOnboarding();
createRecoveryBanner();
createCallPeer();

// Screen share + in-call device settings (ROADMAP P1). UI lives in components.
createScreenShare();
createDevicePicker();
document.querySelector('#studio-view .transport-inputs')?.append(screenShare.button);
document.getElementById('more-list')?.prepend(devicePicker.button);
createCallNetwork();
document.querySelector('#studio-view .dock')?.append(devicePicker.popover);
createNoiseSuppression();
devicePicker.popover.querySelector('.device-note')?.before(noiseSuppression.element);
createRecordingMic();
noiseSuppression.element.before(recordingMic.inCall);
document.getElementById('pcm-audio-toggle')?.closest('label')?.before(recordingMic.greenRoom);
createEffects();
document.getElementById('more-list')?.prepend(effects.menuButton);
createPhoneCamera();
createProductionExtras();
effects.menuButton.after(phoneCamera.menuButton);
document.querySelector('#studio-view .dock')?.append(effects.panel);
document.querySelector('#green-room .device-row')?.after(effects.greenRoomRow);

bindDeviceSelects();
bindDiagnostics();
bindAdmissionBanner();
bindJoin();
bindSignalingLifecycle();

// One handler per envelope type; everything else goes to the feature components.
registerEnvelopeHandlers({
  [TypeAdmission]: env => onAdmission(env.payload as AdmissionPayload),
  [TypeIceCandidate]: env => onIceCandidate(env.payload as IceCandidatePayload),
  [TypeDashboard]: env => renderDashboard(env.payload as DashboardPayload),
  [TypeSdpOffer]: env => onSdpOffer(env.payload as SdpPayload),
  [TypePreflight]: env => onPreflightRequest(env.payload as PreflightPayload),
  [TypeParticipantState]: env => onParticipantState(env.payload as ParticipantStatePayload),
  [TypeSdpAnswer]: env => onSdpAnswer(env.payload as SdpPayload),
  [TypeClockPong]: env => clockSync.handlePong(env.payload as ClockPongPayload),
  [TypeRecordStart]: env => onRecordStart(env.payload as RecordStartPayload),
  [TypeRecordStop]: () => stopLocalRecording(),
  [TypeSessionEnd]: () => onSessionEnd(),
  [TypeChirp]: () => onChirp(),
  // room_state: the recording mic default and a producer's Record button,
  // then CallFeatures and the other components.
  [TypeRoomState]: env => { onRoomState(env.payload as RoomStatePayload); dispatchToComponents(env); },
}, dispatchToComponents);

bindTransfersButtons();
createPreflight();
bindStudioControls();
bindExportModal();
bindMuteControls();
createProducerLink();
bindInvite();
bindHealthAside();
createLeaveCall();

// Start
joinBtn.disabled = true;
// The host home opens the camera only on Ready to join (Home.showReady);
// guests, room and session links open it at once [SL-UX-2].
if (entryMode(location.pathname, location.search).kind !== 'host-home') void initDevices().catch(() => {});
updateConnectionPill();
updateResolutionSpec();

// Dock, More menu and in-call layout [SL-UX-1]; runs after every control is mounted.
mountCallChrome(callChromeOptions());
void annotateSourceLinks();
