// Connection quality, adaptive camera caps and upload pacing policies [SL-P0-8].
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'studiolink-netquality-'));
const load = async name => {
  const out = join(dir, `${name}.mjs`);
  await build({ entryPoints: [new URL(`../src/lib/${name}.ts`, import.meta.url).pathname], bundle: true, format: 'esm', outfile: out });
  return import(out);
};
const nq = await load('netquality');
const rtc = await load('rtc');
test.after(() => rm(dir, { recursive: true, force: true }));

const { sampleFromStats, classifyQuality, QualityTracker, isBandwidthLimited, isUplinkStressed, nextBandwidthStep, initialBandwidthStep, BANDWIDTH_STEP } = nq;
const { callEncodingPolicy, callTierIndex, CALL_TIERS, MAX_CALL_TIER, CALL_AUDIO_MAX_BITRATE_BPS, CALL_AUDIO_MIN_BITRATE_BPS } = rtc;

// A getStats() report for a connected peer, with overridable pieces.
function report({ localType = 'host', address = '192.168.1.20', relayProtocol, rtt = 0.04, aob = 2_500_000, fractionLost = 0, lost = 0, received = 1000, jitter = 0.005, qlr = 'none', dropped = 0, frames = 600, screenQlr } = {}) {
  const r = [
    { id: 'T', type: 'transport', selectedCandidatePairId: 'P' },
    { id: 'P', type: 'candidate-pair', localCandidateId: 'L', remoteCandidateId: 'R', state: 'succeeded', nominated: true, currentRoundTripTime: rtt, availableOutgoingBitrate: aob },
    { id: 'L', type: 'local-candidate', candidateType: localType, address, protocol: relayProtocol ? 'udp' : 'udp', relayProtocol },
    { id: 'R', type: 'remote-candidate', candidateType: 'host', address: '192.168.1.10' },
    { id: 'RI', type: 'remote-inbound-rtp', kind: 'video', fractionLost, roundTripTime: rtt },
    { id: 'IA', type: 'inbound-rtp', kind: 'audio', packetsLost: lost, packetsReceived: received, jitter },
    { id: 'IV', type: 'inbound-rtp', kind: 'video', packetsLost: 0, packetsReceived: 0, framesDropped: dropped, framesReceived: frames },
    { id: 'MS', type: 'media-source', kind: 'video', trackIdentifier: 'cam' },
    { id: 'O', type: 'outbound-rtp', kind: 'video', mediaSourceId: 'MS', qualityLimitationReason: qlr },
  ];
  if (screenQlr) r.push({ id: 'MS2', type: 'media-source', kind: 'video', trackIdentifier: 'screen' }, { id: 'O2', type: 'outbound-rtp', kind: 'video', mediaSourceId: 'MS2', qualityLimitationReason: screenQlr });
  return r;
}

test('sample: path, RTT, estimate, loss deltas, jitter, frames and camera limitation', () => {
  const first = sampleFromStats(report({ lost: 10, received: 990, dropped: 0, frames: 100 }), null);
  assert.equal(first.sample.path, 'direct');
  assert.equal(first.sample.rttMs, 40);
  assert.equal(first.sample.availableOutgoingBps, 2_500_000);
  assert.equal(first.sample.inboundLossPct, undefined, 'no rate without a previous sample');
  const next = sampleFromStats(report({ lost: 40, received: 1960, dropped: 30, frames: 200, fractionLost: 0.06, jitter: 0.05, qlr: 'bandwidth' }), first.counters);
  assert.equal(next.sample.inboundLossPct, 3, '30 lost of 1000 since the last sample');
  assert.equal(next.sample.framesDroppedPct, 30);
  assert.equal(next.sample.outboundLossPct, 6);
  assert.equal(next.sample.jitterMs, 50);
  assert.equal(next.sample.qualityLimitation, 'bandwidth');
});

test('sample: tailnet and TURN paths; a screen sender never sets the camera limitation', () => {
  assert.equal(sampleFromStats(report({ address: '100.101.1.2' }), null).sample.path, 'tailnet');
  const relay = sampleFromStats(report({ localType: 'relay', relayProtocol: 'tcp' }), null).sample;
  assert.equal(relay.path, 'relay');
  assert.equal(relay.protocol, 'tcp');
  const s = sampleFromStats(report({ qlr: 'none', screenQlr: 'bandwidth' }), null, new Set(['screen'])).sample;
  assert.equal(s.qualityLimitation, 'none');
});

test('classify: good, fair and poor thresholds with reasons', () => {
  const good = { path: 'direct', rttMs: 40, availableOutgoingBps: 2_000_000 };
  assert.deepEqual(classifyQuality(good, { connectionState: 'connected' }), { level: 'good', reasons: [] });
  assert.equal(classifyQuality({ ...good, rttMs: 300 }).level, 'fair');
  assert.equal(classifyQuality({ ...good, outboundLossPct: 4 }).level, 'fair');
  assert.equal(classifyQuality({ ...good, jitterMs: 60 }).level, 'fair');
  assert.equal(classifyQuality({ ...good, availableOutgoingBps: 400_000 }).level, 'fair');
  assert.equal(classifyQuality({ ...good, rttMs: 650 }).level, 'poor');
  assert.equal(classifyQuality({ ...good, inboundLossPct: 12 }).level, 'poor');
  assert.equal(classifyQuality({ ...good, availableOutgoingBps: 150_000 }).level, 'poor');
  const poor = classifyQuality({ ...good, rttMs: 600, outboundLossPct: 4 });
  assert.deepEqual(poor.reasons, ['High latency (600 ms)', 'Packet loss 4%']);
  assert.equal(classifyQuality({ path: 'none' }, { connectionState: 'connecting' }).level, 'unknown');
  assert.equal(classifyQuality(good, { connectionState: 'failed' }).level, 'poor');
  assert.equal(classifyQuality({ ...good, path: 'relay' }).level, 'good', 'a relay alone is not a quality problem');
});

test('bandwidth-limited only when the estimate does not clear our own cap', () => {
  const capped = { path: 'direct', qualityLimitation: 'bandwidth', availableOutgoingBps: 3_000_000 };
  assert.equal(isBandwidthLimited(capped, 1_564_000), false, 'our maxBitrate cap, not the network');
  assert.equal(classifyQuality(capped, { sendCapBps: 1_564_000 }).level, 'good');
  assert.equal(isBandwidthLimited({ ...capped, availableOutgoingBps: 900_000 }, 1_564_000), true);
  assert.equal(isBandwidthLimited({ ...capped, availableOutgoingBps: undefined }, 1_564_000), true);
  assert.equal(isBandwidthLimited({ ...capped, qualityLimitation: 'cpu' }, 1_564_000), false);
});

test('tracker: one blip never flips; worse after 2 samples, better after 3', () => {
  const t = new QualityTracker();
  const v = level => ({ level, reasons: [level] });
  assert.equal(t.update(v('good')), 'good');
  assert.equal(t.update(v('poor')), 'good', 'single blip');
  assert.equal(t.update(v('good')), 'good');
  assert.equal(t.update(v('poor')), 'good');
  assert.equal(t.update(v('fair')), 'fair', 'two worse samples: the milder one is shown');
  assert.equal(t.update(v('poor')), 'fair');
  assert.equal(t.update(v('poor')), 'poor');
  assert.equal(t.update(v('good')), 'poor');
  assert.equal(t.update(v('good')), 'poor');
  assert.equal(t.update(v('good')), 'good', 'recovery after three better samples');
  assert.equal(t.update(v('unknown')), 'unknown');
  assert.equal(t.update(v('fair')), 'fair', 'first known sample after unknown is taken as is');
});

test('camera tiers: participant count, relay one tier lower, steps clamp at the floor', () => {
  assert.deepEqual(callEncodingPolicy(2), { maxShortSide: 720, videoMaxBitrateBps: 1_500_000, videoMaxFramerate: 30, audioMaxBitrateBps: CALL_AUDIO_MAX_BITRATE_BPS });
  assert.equal(callEncodingPolicy(4).maxShortSide, 540);
  assert.equal(callEncodingPolicy(6).maxShortSide, 360);
  assert.equal(callEncodingPolicy(2, { path: 'relay' }).maxShortSide, 540);
  assert.equal(callEncodingPolicy(2, { path: 'tailnet' }).maxShortSide, 720);
  assert.equal(callEncodingPolicy(2, { bandwidthStep: 2 }).videoMaxBitrateBps, 500_000);
  assert.equal(callTierIndex(8, { path: 'relay', bandwidthStep: 3 }), MAX_CALL_TIER);
  const floor = callEncodingPolicy(2, { bandwidthStep: 9 });
  assert.deepEqual([floor.maxShortSide, floor.videoMaxBitrateBps, floor.audioMaxBitrateBps], [180, 150_000, CALL_AUDIO_MIN_BITRATE_BPS]);
  for (let i = 1; i < CALL_TIERS.length; i++) assert.ok(CALL_TIERS[i].videoMaxBitrateBps < CALL_TIERS[i - 1].videoMaxBitrateBps);
});

// Runs the step reducer every 2 s over a scripted list of samples.
function run(samples, { startMs = 20_000, state = initialBandwidthStep(), cap = 1_564_000 } = {}) {
  const steps = [];
  let now = startMs;
  for (const sample of samples) {
    state = nextBandwidthStep(state, { nowMs: now, connectedForMs: now, sample, sendCapBps: cap });
    steps.push(state.step);
    now += 2000;
  }
  return { state, steps, now };
}
const congested = { path: 'direct', qualityLimitation: 'bandwidth', availableOutgoingBps: 600_000 };
const clear = { path: 'direct', qualityLimitation: 'none', availableOutgoingBps: 3_000_000 };

test('caps step down after sustained congestion, not on a blip or during startup', () => {
  assert.deepEqual(run([congested, clear, congested, clear]).steps, [0, 0, 0, 0], 'blips');
  assert.deepEqual(run([congested, congested]).steps, [0, 1]);
  const startup = nextBandwidthStep(initialBandwidthStep(), { nowMs: 2000, connectedForMs: 2000, sample: congested, sendCapBps: 1_564_000 });
  assert.equal(startup.step, 0, 'Chrome ramps its estimate after connecting');
  // Repeated congestion keeps stepping, at most every 4 s, up to the max.
  const deep = run(Array(12).fill(congested));
  assert.equal(deep.state.step, BANDWIDTH_STEP.maxStep);
  assert.deepEqual(deep.steps.slice(0, 6), [0, 1, 1, 2, 2, 3]);
  // Loss alone counts as congestion.
  assert.equal(run([{ ...clear, outboundLossPct: 8 }, { ...clear, outboundLossPct: 8 }]).state.step, 1);
});

test('caps recover upward only after sustained clear conditions with headroom', () => {
  const down = run([congested, congested]);
  assert.equal(down.state.step, 1);
  // 10 s of clear samples are needed (first clear sample starts the clock).
  const up = run(Array(6).fill(clear), { startMs: down.now, state: down.state });
  assert.deepEqual(up.steps, [1, 1, 1, 1, 1, 0]);
  // The estimate does not even cover the current cap: stay down.
  const tight = run(Array(10).fill({ ...clear, availableOutgoingBps: 900_000 }), { startMs: down.now, state: down.state, cap: 964_000 });
  assert.equal(tight.state.step, 1);
});

test('a failed upward probe doubles the hold time (no oscillation)', () => {
  let { state, now } = run([congested, congested]);
  ({ state, now } = run(Array(6).fill(clear), { startMs: now, state }));
  assert.equal(state.step, 0);
  ({ state, now } = run([congested, congested], { startMs: now, state }));
  assert.equal(state.step, 1);
  assert.equal(state.upHoldMs, 2 * BANDWIDTH_STEP.baseUpHoldMs);
  const again = run(Array(6).fill(clear), { startMs: now, state });
  assert.equal(again.state.step, 1, 'the base hold is no longer enough');
  const later = run(Array(11).fill(clear), { startMs: now, state });
  assert.equal(later.state.step, 0, '20 s of clear conditions are');
});

test('uploads back off on a stressed uplink, not on latency alone', () => {
  const ok = { path: 'direct', rttMs: 320, availableOutgoingBps: 3_000_000, qualityLimitation: 'none' };
  assert.equal(isUplinkStressed(ok, 1_564_000), false, 'distant guest on a fat pipe');
  assert.equal(isUplinkStressed({ ...ok, outboundLossPct: 3 }, 1_564_000), true);
  assert.equal(isUplinkStressed({ ...ok, availableOutgoingBps: 450_000 }, 1_564_000), true);
  assert.equal(isUplinkStressed({ ...ok, qualityLimitation: 'bandwidth', availableOutgoingBps: 1_200_000 }, 1_564_000), true);
});
