// A fake built-in transcription provider speaking the p2p.video file
// transcription protocol (docs/integrations/TRANSCRIPTS.md) for browser tests
// [SL-P4-3]. It connects like the Mac app (loopback + desktop token) and,
// instead of recognizing speech, reads the job's WAV header and answers with
// progress, one deterministic segment per track and `done`. It honours
// `cancel`. Real recognition is covered by the opt-in Swift test
// (STUDIOLINK_REAL_SPEECH=1) and needs no browser.
import { open, readFile } from 'node:fs/promises';
import path from 'node:path';

/** Duration of a PCM WAV from its RIFF header (fmt + data chunk sizes). */
export async function wavDurationSec(file) {
  const fh = await open(file, 'r');
  try {
    const head = Buffer.alloc(4096);
    const { bytesRead } = await fh.read(head, 0, head.length, 0);
    const b = head.subarray(0, bytesRead);
    if (b.toString('ascii', 0, 4) !== 'RIFF' || b.toString('ascii', 8, 12) !== 'WAVE') throw new Error('not a WAV file');
    let off = 12, byteRate = 0, dataBytes = 0;
    while (off + 8 <= b.length) {
      const id = b.toString('ascii', off, off + 4), size = b.readUInt32LE(off + 4);
      if (id === 'fmt ') byteRate = b.readUInt32LE(off + 16);
      if (id === 'data') { dataBytes = size; break; }
      off += 8 + size + (size % 2);
    }
    if (!byteRate) throw new Error('WAV without fmt chunk');
    if (!dataBytes || dataBytes === 0xffffffff) dataBytes = (await fh.stat()).size - off - 8;
    return dataBytes / byteRate;
  } finally { await fh.close(); }
}

export async function connectFakeTranscriber({ origin, dataDir, state = 'ready', jobMs = 1500, text = name => `Hello from ${name} on the built-in engine.` }) {
  const token = (await readFile(path.join(dataDir, '.desktop_token'), 'utf8')).trim();
  const ws = new WebSocket(origin.replace(/^http/, 'ws') + '/api/v1/transcription/provider', { headers: { Authorization: `Bearer ${token}` } });
  const stats = { ready: false, jobs: [], cancels: 0, errors: [] };
  const timers = new Map();
  const send = m => ws.send(JSON.stringify(m));

  await new Promise((resolve, reject) => {
    ws.onopen = () => send({ t: 'hello', protocol: 1, name: 'Fake built-in transcriber', engine: 'fake-file-asr', languages: ['en-US'], state, message: state === 'ready' ? undefined : 'Speech Recognition is not allowed for p2p.video.' });
    ws.onerror = () => reject(new Error('fake transcriber could not connect'));
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.t === 'ready') { stats.ready = true; resolve(); } };
  });

  ws.onmessage = async e => {
    const m = JSON.parse(e.data);
    if (m.t === 'cancel') {
      stats.cancels++;
      clearTimeout(timers.get(m.jobId)); timers.delete(m.jobId);
      send({ t: 'error', jobId: m.jobId, code: 'cancelled', message: 'Transcription was cancelled.' });
      return;
    }
    if (m.t !== 'job') return;
    const name = path.basename(m.path).replace(/\.[^.]+$/, '');
    stats.jobs.push({ jobId: m.jobId, file: path.basename(m.path), language: m.language, durationSec: m.durationSec });
    let dur;
    try { dur = await wavDurationSec(m.path); } catch (err) {
      stats.errors.push(String(err));
      send({ t: 'error', jobId: m.jobId, code: 'audio_unreadable', message: 'The audio file could not be opened for transcription.' });
      return;
    }
    send({ t: 'progress', jobId: m.jobId, processedSec: +(dur / 2).toFixed(3) });
    timers.set(m.jobId, setTimeout(() => {
      timers.delete(m.jobId);
      const start = Math.min(0.5, dur / 4), end = Math.min(start + 1.5, dur);
      const words = text(name).split(' ').map((w, i, all) => ({ word: w, start: +(start + (end - start) * i / all.length).toFixed(3), end: +(start + (end - start) * (i + 1) / all.length).toFixed(3), confidence: 0.9 }));
      send({ t: 'segment', jobId: m.jobId, segment: { start, end, text: text(name), confidence: 0.9, words } });
      send({ t: 'done', jobId: m.jobId, durationSec: +dur.toFixed(3), locale: m.language || 'en-US', asr: 'fake-file-asr' });
    }, jobMs));
  };

  return { stats, setState: (s, message) => send({ t: 'status', state: s, message }), close: () => ws.close() };
}
