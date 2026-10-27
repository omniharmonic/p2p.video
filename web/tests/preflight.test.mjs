import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/lib/preflight.ts', import.meta.url), 'utf8');
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
const pf = await import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
const fixture = JSON.parse(await readFile(new URL('../../testdata/protocol/preflight.json', import.meta.url), 'utf8'));
const protocolTs = await readFile(new URL('../src/lib/protocol.ts', import.meta.url), 'utf8');
const GB = 1024 ** 3;

test('preflight fixture matches the TypeScript protocol mirror', () => {
  assert.match(protocolTs, /export const TypePreflight = "preflight";/);
  assert.equal(fixture.t, 'preflight');
  for (const key of Object.keys(fixture.payload)) assert.match(protocolTs, new RegExp(`\\b${key}\\??:`), key);
  for (const item of fixture.payload.items) {
    assert.deepEqual(Object.keys(item).sort(), ['detail', 'id', 'label', 'status']);
    assert.ok(['ok', 'warn', 'fail', 'info'].includes(item.status));
  }
  assert.equal(pf.overallStatus(fixture.payload.items), fixture.payload.overall);
});

test('expected take size: 12 Mbps camera + audio ≈ 11 GB for 2 h', () => {
  const bps = pf.expectedBytesPerSecond(12_000_000);
  assert.equal(bps, (12_000_000 + 256_000) / 8);
  assert.ok(Math.abs(bps * 7200 / GB - 10.27) < 0.1);
  assert.equal(pf.expectedBytesPerSecond(12_000_000, { sampleRate: 48000, channels: 1 }), bps + 192_000);
  assert.equal(pf.expectedBytesPerSecond(NaN), bps, 'invalid bitrate falls back to the default');
});

test('storage: whole planned take must fit in the browser allowance', () => {
  const bps = pf.expectedBytesPerSecond(12_000_000);
  const need = bps * 7200;
  assert.equal(pf.evaluateStorage({ quota: need * 2, usage: 0 }, bps).status, 'ok');
  assert.equal(pf.evaluateStorage({ quota: need * 1.1, usage: 0 }, bps).status, 'warn');
  const fail = pf.evaluateStorage({ quota: need, usage: need / 2 }, bps);
  assert.equal(fail.status, 'fail');
  assert.match(fail.detail, /room for about 60 min/);
  assert.equal(pf.evaluateStorage(null, bps).status, 'warn');
  assert.equal(pf.evaluateStorage({ quota: 0 }, bps).status, 'warn');
});

test('device checks', () => {
  const live = { hasTrack: true, live: true, enabled: true };
  assert.equal(pf.evaluateCamera({ ...live, framesAdvancing: true }).status, 'ok');
  assert.equal(pf.evaluateCamera({ ...live, framesAdvancing: false }).status, 'fail');
  assert.equal(pf.evaluateCamera({ ...live, framesAdvancing: null }).status, 'info');
  assert.equal(pf.evaluateCamera({ ...live, enabled: false, framesAdvancing: true }).status, 'warn');
  assert.equal(pf.evaluateCamera({ hasTrack: false, live: false, enabled: false, framesAdvancing: null }).status, 'fail');
  assert.equal(pf.evaluateMic({ ...live, peakDb: -20 }).status, 'ok');
  assert.equal(pf.evaluateMic({ ...live, peakDb: -Infinity }).status, 'fail');
  assert.equal(pf.evaluateMic({ ...live, peakDb: -60 }).status, 'warn');
  assert.equal(pf.evaluateMic({ ...live, peakDb: -0.1 }).status, 'warn');
  assert.equal(pf.evaluateMic({ ...live, peakDb: null }).status, 'info');
  assert.equal(pf.evaluateMic({ ...live, enabled: false, peakDb: null }).status, 'warn');
  assert.equal(pf.evaluateResolution({ width: 1920, height: 1080, frameRate: 30 }).detail, '1920 × 1080 at 30 fps');
  assert.equal(pf.evaluateResolution({ width: 1080, height: 1920 }).status, 'ok');
  assert.equal(pf.evaluateResolution({ width: 1280, height: 720 }).status, 'warn');
  assert.equal(pf.evaluateResolution({ width: 640, height: 480 }).status, 'fail');
  assert.equal(pf.evaluateOPFS(false).status, 'fail');
  assert.equal(pf.evaluatePersist(false).status, 'warn');
  assert.equal(pf.evaluatePersist(true).status, 'ok');
  assert.equal(pf.evaluatePersist(null).status, 'info');
});

test('headphones are a label hint only', () => {
  assert.equal(pf.evaluateHeadphones({ outputs: ['AirPods Pro'] }).status, 'ok');
  assert.equal(pf.evaluateHeadphones({ outputs: ['MacBook Pro Speakers'], input: 'Jabra Evolve2 65' }).status, 'ok');
  assert.equal(pf.evaluateHeadphones({ outputs: ['WH-1000XM4'] }).status, 'ok');
  const speakers = pf.evaluateHeadphones({ outputs: ['MacBook Pro Speakers'], input: 'MacBook Pro Microphone' });
  assert.equal(speakers.status, 'warn');
  assert.match(speakers.detail, /speakers/);
  assert.equal(pf.evaluateHeadphones({ outputs: [] }).status, 'warn');
});

test('connection path from getStats selected pair', () => {
  const stats = (local, remote, viaTransport = true) => [
    { id: 'T', type: 'transport', ...(viaTransport ? { selectedCandidatePairId: 'P' } : {}) },
    { id: 'P', type: 'candidate-pair', localCandidateId: 'L', remoteCandidateId: 'R', currentRoundTripTime: 0.012, nominated: true, state: 'succeeded' },
    { id: 'L', type: 'local-candidate', ...local },
    { id: 'R', type: 'remote-candidate', ...remote },
  ];
  assert.deepEqual(pf.connectionPathFromStats(stats({ candidateType: 'host', protocol: 'udp', address: '192.168.1.4' }, { candidateType: 'host', address: '192.168.1.2' })), { path: 'direct', protocol: 'udp', rttMs: 12 });
  assert.equal(pf.connectionPathFromStats(stats({ candidateType: 'relay', protocol: 'udp', relayProtocol: 'tcp', address: '127.0.0.1' }, { candidateType: 'host' })).path, 'relay');
  assert.equal(pf.connectionPathFromStats(stats({ candidateType: 'relay', relayProtocol: 'tcp' }, {})).protocol, 'tcp');
  assert.equal(pf.connectionPathFromStats(stats({ candidateType: 'host', address: '100.101.1.2' }, { candidateType: 'host', address: '100.70.0.1' })).path, 'tailnet');
  assert.equal(pf.connectionPathFromStats(stats({ candidateType: 'host', address: 'fd7a:115c:a1e0::5' }, {}, false)).path, 'tailnet');
  assert.equal(pf.connectionPathFromStats([]).path, 'none');
  assert.equal(pf.isTailnetAddress('100.63.0.1'), false);
  assert.equal(pf.isTailnetAddress('100.128.0.1'), false);
  assert.equal(pf.evaluatePath({ path: 'none' }, true).status, 'fail');
  assert.equal(pf.evaluatePath({ path: 'none', state: 'failed' }, true).detail, 'Call media not connected (failed)');
  assert.equal(pf.evaluatePath({ path: 'none', state: 'connecting' }, true).status, 'info', 'joining is not a red item');
  assert.equal(pf.evaluatePath({ path: 'none', state: 'new' }, true).status, 'info');
  assert.equal(pf.evaluatePath({ path: 'relay', protocol: 'tcp' }, true).status, 'warn');
  assert.equal(pf.evaluatePath({ path: 'direct', protocol: 'udp', rttMs: 3 }, true).detail, 'Direct UDP · 3 ms RTT');
  assert.equal(pf.evaluatePath(null, false).status, 'info');
});

test('power is a hint and never red', () => {
  assert.equal(pf.evaluatePower(null).status, 'info');
  assert.equal(pf.evaluatePower({ charging: true, level: 0.1 }).status, 'ok');
  assert.equal(pf.evaluatePower({ charging: false, level: 0.9 }).status, 'warn');
  assert.equal(pf.evaluatePower({ charging: false, level: 0.05 }).status, 'warn');
});

test('blocking problems aggregate this device and every participant', () => {
  const local = [{ id: 'mic', label: 'Microphone', status: 'fail', detail: 'No signal' }, { id: 'persist', label: 'Persistent storage', status: 'warn' }];
  const remote = [
    { name: 'Guest A', preflight: { items: [{ id: 'camera', label: 'Camera', status: 'fail' }, { id: 'path', label: 'Connection', status: 'warn' }] } },
    { name: 'Guest B' },
  ];
  const problems = pf.blockingProblems(local, remote);
  assert.deepEqual(problems.map(p => `${p.who}:${p.item.id}`), ['You:mic', 'Guest A:camera']);
  assert.equal(pf.overallStatus([{ status: 'info' }, { status: 'ok' }]), 'ok');
  assert.equal(pf.overallStatus([{ status: 'warn' }, { status: 'info' }]), 'warn');
});

test('host chip: a producer’s own checklist never turns the room red [SL-QA-1]', () => {
  const fail = { overall: 'fail' }, warn = { overall: 'warn' };
  assert.equal(pf.remoteWorst([]), 'ok');
  assert.equal(pf.remoteWorst([{ role: 'producer', preflight: fail }]), 'ok', 'producer is never recorded');
  assert.equal(pf.remoteWorst([{ role: 'producer', preflight: fail }, { role: 'guest', preflight: warn }]), 'warn');
  assert.equal(pf.remoteWorst([{ role: 'guest', preflight: fail }, { role: 'guest' }]), 'fail');
});
