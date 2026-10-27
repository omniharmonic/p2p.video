// A fake caption provider speaking the p2p.video provider protocol
// (docs/integrations/CAPTIONS.md) for browser tests [SL-P4-2]. It connects
// like the Mac app (loopback + desktop token) and, instead of recognizing
// speech, classifies each stream's audio as a steady "Tone" or "Speech" by
// zero-crossing rate, so a test can prove which participant's audio reached
// which caption. Results: a partial after ~1 s of audio, a final after ~2 s,
// repeated every ~3 s per stream.
import { readFile } from 'node:fs/promises';
import path from 'node:path';

export async function connectFakeProvider({ origin, dataDir, toneHz = 600 }) {
  const token = (await readFile(path.join(dataDir, '.desktop_token'), 'utf8')).trim();
  const ws = new WebSocket(origin.replace(/^http/, 'ws') + '/api/v1/captions/provider', { headers: { Authorization: `Bearer ${token}` } });
  ws.binaryType = 'arraybuffer';
  const streams = new Map();
  const stats = { starts: 0, ends: 0, frames: 0, results: 0, ready: false, languages: [] };
  const send = m => ws.send(JSON.stringify(m));
  const lo = toneHz * 2 * 0.85, hi = toneHz * 2 * 1.15; // crossings per second

  await new Promise((resolve, reject) => {
    ws.onopen = () => send({ t: 'hello', protocol: 1, name: 'Fake test provider', engine: 'fake-zcr', languages: ['en-US', 'en-GB'] });
    ws.onerror = () => reject(new Error('fake provider could not connect'));
    ws.onmessage = e => {
      if (typeof e.data !== 'string') return;
      const m = JSON.parse(e.data);
      if (m.t === 'ready') { stats.ready = true; resolve(); }
    };
  });

  ws.onmessage = e => {
    if (typeof e.data === 'string') {
      const m = JSON.parse(e.data);
      if (m.t === 'stream_start') {
        stats.starts++;
        stats.languages.push(m.language);
        streams.set(m.streamId, { samples: 0, tone: 0, voiced: 0, seg: 1, partialSent: false, segStart: 0 });
      } else if (m.t === 'stream_end') {
        stats.ends++;
        streams.delete(m.streamId);
      }
      return;
    }
    const b = new Uint8Array(e.data);
    if (b[0] !== 1) throw new Error('bad frame version');
    const key = new TextDecoder().decode(b.subarray(2, 2 + b[1]));
    const pcm = new Int16Array(b.slice(2 + b[1]).buffer);
    const s = streams.get(key);
    if (!s) throw new Error(`audio for unknown stream ${key}`);
    stats.frames++;
    let crossings = 0, energy = 0;
    for (let i = 0; i < pcm.length; i++) {
      energy += pcm[i] * pcm[i];
      if (i && (pcm[i] >= 0) !== (pcm[i - 1] >= 0)) crossings++;
    }
    const rms = Math.sqrt(energy / pcm.length);
    const zcr = crossings / (pcm.length / 16000);
    if (rms > 30) { s.voiced++; if (zcr > lo && zcr < hi) s.tone++; }
    if (process.env.CAPTIONS_DEBUG && stats.frames % 50 === 0) console.log('fake provider', key, Math.round(rms), Math.round(zcr));
    s.samples += pcm.length;
    const since = s.samples - s.segStart;
    const kind = s.voiced && s.tone / s.voiced > 0.6 ? 'Tone' : 'Speech';
    const startMs = Math.round(s.segStart / 16);
    if (!s.partialSent && since >= 16000) {
      s.partialSent = true;
      stats.results++;
      send({ t: 'result', streamId: key, segmentId: String(s.seg), text: `${kind} heard`, startMs, endMs: Math.round(s.samples / 16) });
    } else if (since >= 32000) {
      stats.results++;
      send({ t: 'result', streamId: key, segmentId: String(s.seg), text: `${kind} heard on this stream.`, isFinal: true, startMs, endMs: Math.round(s.samples / 16) });
      s.seg++; s.partialSent = false; s.segStart = s.samples + 16000; // ~1 s pause between segments
    }
  };

  return { stats, close: () => ws.close() };
}
