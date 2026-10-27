import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

async function load(path) {
  const source = await readFile(new URL(path, import.meta.url), 'utf8');
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
}
const { WebRTCClient, callEncodingPolicy, screenEncodingPolicy, applyCallParameters, RENEGOTIATION_TIMEOUT_MS } = await load('../src/lib/rtc.ts');
const protocol = await load('../src/lib/protocol.ts');

// A PeerConnection mock with a real signaling-state machine, enough to model
// client renegotiation and glare with the SFU's offers.
class Peer {
  signalingState = 'stable';
  remoteDescription = null;
  localDescription = null;
  senders = [];
  offers = 0;
  closed = false;
  transceivers = [];
  addTransceiver(trackOrKind, init) {
    const t = { init, sender: typeof trackOrKind === 'string' ? null : new Sender(trackOrKind) };
    if (t.sender) this.senders.push(t.sender);
    this.transceivers.push(t);
    return t;
  }
  addTrack(track) { const s = new Sender(track); this.senders.push(s); return s; }
  removeTrack(sender) { this.senders = this.senders.filter(s => s !== sender); }
  getSenders() { return this.senders; }
  async createOffer() { this.offers++; return { type: 'offer', sdp: `client-offer-${this.offers}` }; }
  async createAnswer() { return { type: 'answer', sdp: 'client-answer' }; }
  async setLocalDescription(d) {
    if (d.type === 'rollback') { assert.equal(this.signalingState, 'have-local-offer'); this.localDescription = null; this.signalingState = 'stable'; return; }
    this.localDescription = d;
    this.signalingState = d.type === 'offer' ? 'have-local-offer' : 'stable';
  }
  async setRemoteDescription(d) {
    if (d.type === 'offer') assert.equal(this.signalingState, 'stable', 'offer applied only when stable');
    if (d.type === 'answer') assert.equal(this.signalingState, 'have-local-offer', 'answer only for our offer');
    this.remoteDescription = d;
    this.signalingState = d.type === 'offer' ? 'have-remote-offer' : 'stable';
  }
  async addIceCandidate() {}
  close() { this.closed = true; }
}
class Sender {
  constructor(track) { this.track = track; this.params = { encodings: [{}] }; }
  getParameters() { return structuredClone(this.params); }
  async setParameters(p) { this.params = p; }
  async replaceTrack(t) { this.track = t; }
}
globalThis.RTCPeerConnection = Peer;
const track = (kind, id, settings = {}) => ({ kind, id, getSettings: () => settings });
const stream = (...tracks) => ({ getTracks: () => tracks });

test('screen policy keeps text sharp: ≤1080p, 2.5 Mbps at ≤15 fps for small rooms, maintain-resolution', () => {
  for (const n of [1, 2, 3, 4]) {
    const p = screenEncodingPolicy(n);
    assert.deepEqual([p.maxShortSide, p.videoMaxBitrateBps, p.videoMaxFramerate, p.degradationPreference], [1080, 2_500_000, 15, 'maintain-resolution'], `room of ${n}`);
  }
  assert.deepEqual([screenEncodingPolicy(6).videoMaxBitrateBps, screenEncodingPolicy(6).videoMaxFramerate], [1_500_000, 10]);
  assert.deepEqual([screenEncodingPolicy(12).maxShortSide, screenEncodingPolicy(12).videoMaxBitrateBps], [720, 1_000_000]);
  for (let n = 1; n < 16; n++) {
    assert.ok(screenEncodingPolicy(n + 1).videoMaxBitrateBps <= screenEncodingPolicy(n).videoMaxBitrateBps, 'monotonic bitrate');
    assert.ok(screenEncodingPolicy(n).videoMaxBitrateBps > callEncodingPolicy(n).videoMaxBitrateBps, 'screen gets more than a camera');
  }
  for (const bad of [0, -1, NaN, Infinity]) assert.equal(screenEncodingPolicy(bad).maxShortSide, 1080);
  // A 4K screen is scaled to 1080p; a 1080p screen is sent as is.
  const uhd = applyCallParameters({ encodings: [{}] }, 'video', screenEncodingPolicy(3), { width: 3840, height: 2160 });
  assert.deepEqual(uhd.encodings, [{ maxBitrate: 2_500_000, maxFramerate: 15, scaleResolutionDownBy: 2 }]);
  assert.equal(uhd.degradationPreference, 'maintain-resolution');
  assert.equal(applyCallParameters({ encodings: [{}] }, 'video', screenEncodingPolicy(3), { width: 1920, height: 1080 }).encodings[0].scaleResolutionDownBy, 1);
  // Camera default unchanged.
  assert.equal(applyCallParameters({ encodings: [{}] }, 'video', callEncodingPolicy(3)).degradationPreference, 'maintain-framerate');
});

test('screen senders get the screen policy, camera senders keep the call policy', async () => {
  const client = new WebRTCClient(() => {});
  client.init();
  client.pc.remoteDescription = { type: 'answer' };
  const cam = track('video', 'cam', { width: 1920, height: 1080 });
  client.addTracks(stream(cam, track('audio', 'mic')));
  const screen = track('video', 'scr', { width: 2560, height: 1440 });
  const sysAudio = track('audio', 'sys');
  client.addScreen(stream(screen, sysAudio), false);
  await client.setParticipantCount(3);
  const by = id => client.pc.senders.find(s => s.track.id === id).params;
  assert.equal(by('cam').encodings[0].maxBitrate, 900_000);
  assert.equal(by('cam').degradationPreference, 'maintain-framerate');
  assert.equal(by('scr').encodings[0].maxBitrate, 2_500_000);
  assert.equal(by('scr').encodings[0].maxFramerate, 15);
  assert.equal(by('scr').encodings[0].scaleResolutionDownBy, 1.333);
  assert.equal(by('scr').degradationPreference, 'maintain-resolution');
  assert.equal(by('sys').encodings[0].maxBitrate, 96_000);
  assert.equal(by('mic').encodings[0].maxBitrate, 64_000);
  const screenTransceivers = client.pc.transceivers.filter(t => t.sender);
  assert.deepEqual(screenTransceivers.map(t => [t.sender.track.id, t.init.direction]), [['scr', 'sendrecv'], ['sys', 'sendrecv']], 'screen gets its own transceivers, never a reused subscription m-line');
  client.removeScreen();
  assert.deepEqual(client.pc.senders.map(s => s.track.id), ['cam', 'mic']);
  assert.equal(client.hasScreen, false);
});

async function joined() {
  const client = new WebRTCClient(() => {});
  const offers = [];
  client.onRenegotiationOffer = sdp => offers.push(sdp);
  client.init();
  await client.createOffer();
  await client.handleAnswer('server-answer');
  return { client, offers };
}
const tick = () => new Promise(r => setTimeout(r, 0));

test('adding a screen renegotiates the existing PeerConnection, never replacing it', async () => {
  const { client, offers } = await joined();
  const pc = client.pc;
  client.addScreen(stream(track('video', 'scr')));
  await tick();
  assert.deepEqual(offers, ['client-offer-2']);
  assert.equal(client.pc, pc);
  await client.handleAnswer('server-reneg-answer');
  assert.equal(pc.signalingState, 'stable');
  // A second change (stop) renegotiates again.
  client.removeScreen();
  await tick();
  assert.deepEqual(offers, ['client-offer-2', 'client-offer-3']);
});

test('glare: our renegotiation offer rolls back for the server offer, then is re-sent', async () => {
  const { client, offers } = await joined();
  client.addScreen(stream(track('video', 'scr')));
  await tick();
  assert.equal(client.pc.signalingState, 'have-local-offer');
  // The SFU had its own subscription offer in flight: it ignores ours.
  const answer = await client.handleOffer('server-subscription-offer');
  assert.equal(answer, 'client-answer', 'server offer is answered, not dropped');
  assert.equal(client.pc.remoteDescription.sdp, 'server-subscription-offer');
  assert.equal(client.pc.signalingState, 'stable');
  assert.equal(offers.length, 1, 're-offer waits until the answer is sent');
  await tick();
  await tick();
  assert.equal(offers.length, 2, 'the screen offer is retried once stable');
  assert.equal(client.pc.senders.length, 1, 'rollback keeps the screen sender');
});

test('renegotiation requested before the first answer waits for it', async () => {
  const client = new WebRTCClient(() => {});
  const offers = [];
  client.onRenegotiationOffer = sdp => offers.push(sdp);
  client.init();
  await client.createOffer();
  client.addScreen(stream(track('video', 'scr')));
  await tick();
  assert.equal(offers.length, 0);
  await client.handleAnswer('server-answer');
  await tick();
  assert.equal(offers.length, 1);
});

test('an unanswered renegotiation falls back to media recovery', async (t) => {
  const { client } = await joined();
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let failures = 0;
  client.onMediaFailure = () => failures++;
  client.requestRenegotiation();
  // Guarded offer/setLocalDescription settle through Promise.race; flush enough
  // microtasks for the timeout to be armed before the mocked clock moves.
  for (let i = 0; i < 20; i++) await Promise.resolve();
  t.mock.timers.tick(RENEGOTIATION_TIMEOUT_MS - 1);
  assert.equal(failures, 0);
  t.mock.timers.tick(1);
  assert.equal(failures, 1);
});

test('an answer without an outstanding offer is ignored', async () => {
  const { client } = await joined();
  await client.handleAnswer('late-duplicate');
  assert.equal(client.pc.remoteDescription.sdp, 'server-answer');
});

test('replaceSenderTrack swaps a device without renegotiation', async () => {
  const { client, offers } = await joined();
  const oldCam = track('video', 'cam-a', { width: 1280, height: 720 });
  client.addTracks(stream(oldCam));
  const newCam = track('video', 'cam-b', { width: 1920, height: 1080 });
  assert.equal(await client.replaceSenderTrack(oldCam, newCam), true);
  assert.equal(client.pc.senders[0].track, newCam);
  assert.equal(await client.replaceSenderTrack(track('video', 'unknown'), newCam), false);
  await tick();
  assert.equal(offers.length, 0);
});

test('protocol.ts screen constants mirror protocol.go', async () => {
  const go = await readFile(new URL('../../host/internal/signaling/protocol.go', import.meta.url), 'utf8');
  assert.match(go, new RegExp(`ScreenStreamSuffix = "${protocol.ScreenStreamSuffix}"`));
  assert.match(go, new RegExp(`TypeScreenStop = "${protocol.TypeScreenStop}"`));
  assert.match(go, new RegExp(`MaxScreenTrackIDs  = ${protocol.MaxScreenTrackIDs} `));
  const sfu = await readFile(new URL('../../host/internal/sfu/sfu.go', import.meta.url), 'utf8');
  assert.match(sfu, new RegExp(`ScreenStreamSuffix = "${protocol.ScreenStreamSuffix}"`));
});
