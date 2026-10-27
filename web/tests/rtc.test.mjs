import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/lib/rtc.ts', import.meta.url), 'utf8');
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
const { WebRTCClient, PeerClosedError, callEncodingPolicy, scaleDownFactor, applyCallParameters, MEDIA_DISCONNECT_GRACE_MS } = await import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
class Peer {
  remoteDescription = null;
  localDescription = null;
  candidates = [];
  transceivers = [];
  closed = false;
  addTransceiver(kind, init) { this.transceivers.push({kind,...init}); }
  async setRemoteDescription(sdp) { this.remoteDescription = sdp; }
  async setLocalDescription(sdp) { this.localDescription = sdp; }
  async createOffer() { return { type: 'offer', sdp: 'initial' }; }
  async createAnswer() { return { type: 'answer', sdp: 'subscription-answer' }; }
  async addIceCandidate(candidate) { assert.ok(this.remoteDescription); this.candidates.push(candidate); }
  close() { this.closed = true; }
}
globalThis.RTCPeerConnection = Peer;

test('buffers early ICE, accepts a server subscription offer and returns its answer', async () => {
  const client = new WebRTCClient(() => {});
  client.init();
  assert.deepEqual(client.pc.transceivers.map(t => t.kind), ['audio', 'video']);
  await client.createOffer();
  const candidate = { candidate: 'early-candidate' };
  await client.addIceCandidate(candidate);
  assert.equal(client.pc.candidates.length, 0);
  await client.handleAnswer('initial-answer');
  assert.deepEqual(client.pc.candidates, [candidate]);
  assert.equal(await client.handleOffer('new-publisher'), 'subscription-answer');
  assert.equal(client.pc.remoteDescription.sdp, 'new-publisher');
});

test('reinitializing closes old peers and ignores their late media and ICE', () => {
  const tracks = [], candidates = [];
  const client = new WebRTCClient((...args) => tracks.push(args), c => candidates.push(c));
  client.init();
  const previous = client.pc;
  const lateTrack = previous.ontrack, lateCandidate = previous.onicecandidate;
  client.init();
  assert.equal(previous.closed, true);
  lateTrack({ track: {}, streams: [{}] });
  lateCandidate({ candidate: { toJSON: () => ({ candidate: 'stale' }) } });
  assert.equal(tracks.length, 0);
  assert.equal(candidates.length, 0);
  const streamA = {id:'alice'}, streamB = {id:'bob'};
  client.pc.ontrack({ track: {kind:'audio'}, streams: [streamA] });
  client.pc.ontrack({ track: {kind:'audio'}, streams: [streamB] });
  assert.equal(tracks[0][1], streamA);
  assert.equal(tracks[1][1], streamB);
});

test('call encoding policy scales resolution and bitrate down as the room grows', () => {
  const table = [1, 2, 3, 4, 5, 6, 12].map(n => [n, callEncodingPolicy(n)]);
  for (const [n, p] of table) {
    assert.equal(p.audioMaxBitrateBps, 64_000, `audio cap for ${n}`);
    if (n <= 2) assert.deepEqual([p.maxShortSide, p.videoMaxBitrateBps, p.videoMaxFramerate], [720, 1_500_000, 30]);
    else if (n <= 4) assert.deepEqual([p.maxShortSide, p.videoMaxBitrateBps, p.videoMaxFramerate], [540, 900_000, 30]);
    else assert.deepEqual([p.maxShortSide, p.videoMaxBitrateBps, p.videoMaxFramerate], [360, 500_000, 24]);
  }
  // Monotonic: more participants never raise the per-sender budget.
  for (let n = 1; n < 12; n++) assert.ok(callEncodingPolicy(n + 1).videoMaxBitrateBps <= callEncodingPolicy(n).videoMaxBitrateBps);
  for (const bad of [0, -3, NaN, Infinity]) assert.equal(callEncodingPolicy(bad).maxShortSide, 720);
});

test('scale factor targets the short side, handles portrait and never upscales', () => {
  assert.equal(scaleDownFactor(1920, 1080, 720), 1.5);
  assert.equal(scaleDownFactor(1080, 1920, 720), 1.5);
  assert.equal(scaleDownFactor(1920, 1080, 540), 2);
  assert.equal(scaleDownFactor(1920, 1080, 360), 3);
  assert.equal(scaleDownFactor(1280, 720, 720), 1);
  assert.equal(scaleDownFactor(640, 480, 720), 1);
  assert.equal(scaleDownFactor(undefined, undefined, 720), 1);
});

test('call parameters cap video and audio senders without changing encoding count', () => {
  const video = applyCallParameters({ encodings: [{ active: true }] }, 'video', callEncodingPolicy(4), { width: 1920, height: 1080 });
  assert.deepEqual(video.encodings, [{ active: true, maxBitrate: 900_000, maxFramerate: 30, scaleResolutionDownBy: 2 }]);
  assert.equal(video.degradationPreference, 'maintain-framerate');
  const audio = applyCallParameters({ encodings: [{}] }, 'audio', callEncodingPolicy(2));
  assert.deepEqual(audio.encodings, [{ maxBitrate: 64_000 }]);
  assert.equal(audio.degradationPreference, undefined);
});

class Sender {
  constructor(kind, settings, rejectDegradation = false) {
    this.track = { kind, getSettings: () => settings };
    this.params = { encodings: [{}] };
    this.rejectDegradation = rejectDegradation;
  }
  getParameters() { return structuredClone(this.params); }
  async setParameters(p) {
    if (this.rejectDegradation && p.degradationPreference) throw new Error('unsupported');
    this.params = p;
  }
}

test('participant count changes re-apply caps to live call senders only', async () => {
  const client = new WebRTCClient(() => {});
  client.init();
  const video = new Sender('video', { width: 1920, height: 1080 }, true), audio = new Sender('audio', {});
  const idle = { track: null, getParameters() { throw new Error('idle sender touched'); } };
  client.pc.getSenders = () => [video, audio, idle];
  await client.setParticipantCount(2);
  assert.equal(video.params.encodings[0].maxBitrate, 1_500_000);
  assert.equal(video.params.encodings[0].scaleResolutionDownBy, 1.5);
  assert.equal(video.params.degradationPreference, undefined, 'falls back when degradationPreference is rejected');
  assert.equal(audio.params.encodings[0].maxBitrate, 64_000);
  await client.setParticipantCount(5);
  assert.equal(video.params.encodings[0].maxBitrate, 500_000);
  assert.equal(video.params.encodings[0].scaleResolutionDownBy, 3);
  assert.equal(video.params.encodings[0].maxFramerate, 24);
});

test('media failure fires once on failed, and after the grace period when disconnected', (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const client = new WebRTCClient(() => {});
  let failures = 0;
  client.onMediaFailure = () => failures++;
  const transition = (state) => { client.pc.connectionState = state; client.pc.onconnectionstatechange(); };
  client.init();
  transition('disconnected');
  t.mock.timers.tick(MEDIA_DISCONNECT_GRACE_MS - 1);
  transition('connected'); // a transient blip recovers on its own
  t.mock.timers.tick(MEDIA_DISCONNECT_GRACE_MS);
  assert.equal(failures, 0);
  transition('disconnected');
  t.mock.timers.tick(MEDIA_DISCONNECT_GRACE_MS);
  assert.equal(failures, 1);
  transition('failed');
  assert.equal(failures, 1, 'one report per PeerConnection');
  client.init();
  transition('failed');
  assert.equal(failures, 2);
  const stale = client.pc;
  client.init();
  assert.equal(stale.onconnectionstatechange, null, 'replaced peers are detached');
  assert.equal(failures, 2);
});

test('server offers are ignored while our recovery offer is pending (glare)', async () => {
  const client = new WebRTCClient(() => {});
  client.init();
  await client.createOffer();
  client.pc.signalingState = 'have-local-offer';
  assert.equal(await client.handleOffer('stale-subscription'), null);
  assert.equal(client.pc.remoteDescription, null);
  client.pc.signalingState = 'stable';
  assert.equal(await client.handleOffer('fresh-subscription'), 'subscription-answer');
});

// Chromium never settles an SDP operation whose PeerConnection is closed while
// it runs. Closing/replacing the peer must reject it so the client's serialized
// signaling chain (and the sender-policy chain) keep moving [SL-P0-7].
test('closing a peer rejects its in-flight SDP, ICE and sender operations', async () => {
  const never = () => new Promise(() => {});
  const client = new WebRTCClient(() => {});
  client.init();
  const hung = client.pc;
  hung.setRemoteDescription = never;
  const offer = client.handleOffer('server-offer');
  client.close();
  await assert.rejects(offer, PeerClosedError);

  client.init();
  const second = client.pc;
  second.createAnswer = never;
  const answer = client.handleOffer('server-offer');
  await new Promise(r => setTimeout(r, 0));
  client.init(); // media recovery replaces the peer mid-answer
  await assert.rejects(answer, PeerClosedError);
  assert.notEqual(client.pc, second);
  assert.equal(await client.handleOffer('next-offer'), 'subscription-answer', 'new peer works after the stale one was abandoned');

  const video = new Sender('video', { width: 1920, height: 1080 });
  video.setParameters = never;
  client.pc.getSenders = () => [video];
  const policy = client.setParticipantCount(3);
  await new Promise(r => setTimeout(r, 0));
  client.init();
  await policy; // resolves instead of hanging forever
  const fresh = new Sender('audio', {});
  client.pc.getSenders = () => [fresh];
  await client.setParticipantCount(2);
  assert.equal(fresh.params.encodings[0].maxBitrate, 64_000, 'policy chain is not wedged by the closed peer');
  assert.ok(hung.closed && second.closed);
});
