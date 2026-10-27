// The WKWebView capability probe page (SL-P2-1 spike). It exercises every web
// platform feature the host studio uses, on the same loopback origin the studio
// is served from, and reports through `window.webkit.messageHandlers.probe`.
enum ProbePage {
    static let html = #"""
<!doctype html>
<html><head><meta charset="utf-8"><title>p2p.video WKWebView probe</title>
<style>body{font:13px -apple-system;margin:16px;background:#111;color:#ddd}li.ok{color:#7d7}li.bad{color:#f88}</style>
</head><body><h3>p2p.video WKWebView capability probe</h3><ul id="out"></ul>
<script>
const checks = [];
const out = document.getElementById('out');
function rec(name, ok, detail, required) {
  checks.push({name, ok: !!ok, required: !!required, detail: String(detail ?? '')});
  const li = document.createElement('li'); li.className = ok ? 'ok' : 'bad';
  li.textContent = `${ok ? 'yes' : 'NO '} ${name}${required ? ' (required)' : ''}: ${detail ?? ''}`; out.appendChild(li);
}
const withTimeout = (p, ms, label) => Promise.race([p, new Promise((_, rej) => setTimeout(() => rej(new Error(`${label} timed out after ${ms} ms`)), ms))]);
function synthStream() {
  const canvas = document.createElement('canvas'); canvas.width = 320; canvas.height = 240;
  const g = canvas.getContext('2d'); let f = 0;
  const timer = setInterval(() => { g.fillStyle = `hsl(${(f++ * 7) % 360} 60% 50%)`; g.fillRect(0, 0, 320, 240); }, 33);
  const ctx = new AudioContext(); const osc = ctx.createOscillator(); const dst = ctx.createMediaStreamDestination();
  osc.connect(dst); osc.start();
  const s = new MediaStream([...canvas.captureStream(30).getVideoTracks(), ...dst.stream.getAudioTracks()]);
  s._cleanup = () => { clearInterval(timer); osc.stop(); ctx.close(); };
  return s;
}
async function run() {
  rec('secureContext', window.isSecureContext, location.origin, true);
  rec('pageVisible', document.visibilityState === 'visible', `visibilityState=${document.visibilityState} hasFocus=${document.hasFocus()}`, false);
  // getUserMedia: real devices, through the native permission delegate and TCC.
  let stream = null, streamSource = 'none';
  if (navigator.mediaDevices?.getUserMedia) {
    try {
      stream = await withTimeout(navigator.mediaDevices.getUserMedia({audio: true, video: {width: 1280, height: 720}}), 15000, 'getUserMedia');
      streamSource = 'camera+microphone';
      const v = stream.getVideoTracks()[0]?.getSettings?.() ?? {};
      rec('getUserMedia', true, `${stream.getAudioTracks().length} audio, ${stream.getVideoTracks().length} video, ${v.width ?? '?'}x${v.height ?? '?'}`, true);
    } catch (e) { rec('getUserMedia', false, `${e.name || 'Error'}: ${e.message}`, true); }
  } else { rec('getUserMedia', false, 'navigator.mediaDevices.getUserMedia missing', true); }
  try { const d = await navigator.mediaDevices.enumerateDevices(); rec('enumerateDevices', true, d.map(x => x.kind).join(',') || 'none', false); }
  catch (e) { rec('enumerateDevices', false, e.message, false); }
  rec('getDisplayMedia', typeof navigator.mediaDevices?.getDisplayMedia === 'function', 'API present (screen share; not exercised)', false);
  let canvasCapture = false;
  try { const c = document.createElement('canvas'); canvasCapture = typeof c.captureStream === 'function' && c.captureStream(1).getVideoTracks().length === 1; } catch {}
  rec('canvasCaptureStream', canvasCapture, 'livestream compositor', false);
  if (!stream) { try { stream = synthStream(); streamSource = 'synthetic canvas+oscillator'; } catch (e) { rec('syntheticStream', false, e.message, false); } }
  // MediaRecorder with the studio's container candidates (web/src/lib/recorder.ts).
  const candidates = ['video/webm;codecs=vp9,opus', 'video/webm', 'video/mp4', 'audio/webm;codecs=opus', 'audio/mp4', 'video/webm;codecs=h264,opus', 'video/mp4;codecs=avc1,mp4a'];
  const supported = typeof MediaRecorder !== 'undefined' ? candidates.filter(t => MediaRecorder.isTypeSupported(t)) : [];
  rec('mediaRecorderStudioMime', supported.some(t => t.startsWith('video/')) && supported.some(t => t.startsWith('audio/')), supported.join(' | ') || 'none', true);
  if (typeof MediaRecorder !== 'undefined' && stream) {
    try {
      const mime = supported.find(t => t.startsWith('video/'));
      const r = new MediaRecorder(stream, mime ? {mimeType: mime} : undefined);
      const chunks = [];
      r.ondataavailable = e => { if (e.data.size) chunks.push(e.data); };
      const stopped = new Promise(res => r.onstop = res);
      r.start(250); await new Promise(res => setTimeout(res, 1500)); r.stop(); await withTimeout(stopped, 5000, 'MediaRecorder stop');
      const bytes = chunks.reduce((n, b) => n + b.size, 0);
      rec('mediaRecorder', bytes > 0, `${bytes} bytes as ${r.mimeType} from ${streamSource}`, true);
    } catch (e) { rec('mediaRecorder', false, `${e.name}: ${e.message}`, true); }
  } else { rec('mediaRecorder', false, typeof MediaRecorder === 'undefined' ? 'MediaRecorder missing' : 'no stream', true); }
  // OPFS as web/src/lib/opfs.ts uses it.
  if (navigator.storage?.getDirectory) {
    try {
      const root = await navigator.storage.getDirectory();
      rec('opfsGetDirectory', true, 'navigator.storage.getDirectory()', true);
      const dir = await root.getDirectoryHandle('probe', {create: true});
      const fh = await dir.getFileHandle('chunk.bin', {create: true});
      if (typeof fh.createWritable === 'function') {
        const w = await fh.createWritable(); await w.write(new Uint8Array([1, 2, 3, 4])); await w.close();
        const size = (await fh.getFile()).size;
        rec('opfsCreateWritable', size === 4, `wrote and read back ${size} bytes`, true);
      } else { rec('opfsCreateWritable', false, 'FileSystemFileHandle.createWritable missing', true); }
      await root.removeEntry('probe', {recursive: true});
      try { const est = await navigator.storage.estimate(); rec('storageEstimate', true, `quota ${Math.round((est.quota || 0) / 1e9)} GB`, false); } catch (e) { rec('storageEstimate', false, e.message, false); }
      try { rec('storagePersist', await navigator.storage.persist(), 'navigator.storage.persist()', false); } catch (e) { rec('storagePersist', false, e.message, false); }
    } catch (e) { rec('opfsGetDirectory', false, `${e.name}: ${e.message}`, true); rec('opfsCreateWritable', false, 'not reached', true); }
  } else { rec('opfsGetDirectory', false, 'navigator.storage.getDirectory missing', true); rec('opfsCreateWritable', false, 'not reached', true); }
  // AudioWorklet as web/src/lib/pcm-recorder.ts uses it.
  try {
    const ctx = new AudioContext();
    if (!ctx.audioWorklet) throw new Error('AudioContext.audioWorklet missing');
    const src = `registerProcessor('probe', class extends AudioWorkletProcessor { process(i){ this.port.postMessage(i[0]?.length ?? 0); return false; } });`;
    await ctx.audioWorklet.addModule(URL.createObjectURL(new Blob([src], {type: 'application/javascript'})));
    const node = new AudioWorkletNode(ctx, 'probe');
    rec('audioWorklet', true, `module loaded, node created at ${ctx.sampleRate} Hz`, true);
    node.disconnect(); await ctx.close();
  } catch (e) { rec('audioWorklet', false, `${e.name}: ${e.message}`, true); }
  rec('webCodecs', typeof VideoEncoder === 'function', 'VideoEncoder (not used by the studio today)', false);
  // WebSocket + host session: the real host path against the local daemon.
  try {
    const resp = await fetch('/api/v1/sessions', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({title: 'WKWebView probe'})});
    if (!resp.ok) throw new Error(`create session HTTP ${resp.status}`);
    const sess = await resp.json();
    const ws = new WebSocket(`ws://${location.host}/api/v1/ws?session=${encodeURIComponent(sess.id)}&token=${encodeURIComponent(sess.hostToken)}`);
    const opened = await withTimeout(new Promise((res, rej) => { ws.onopen = () => res(true); ws.onerror = () => rej(new Error('socket error')); }), 5000, 'WebSocket');
    // Envelope shape from host/internal/signaling/protocol.go: {t, payload}.
    ws.send(JSON.stringify({t: 'hello', payload: {name: 'Probe', role: 'host'}}));
    const admitted = await withTimeout(new Promise(res => { ws.onmessage = ev => { try { const m = JSON.parse(ev.data); if (m.t === 'admission') res(m.payload?.status); } catch {} }; }), 5000, 'admission');
    rec('webSocket', opened && admitted === 'admitted', `host socket opened; admission=${admitted}`, true);
    ws.close();
  } catch (e) { rec('webSocket', false, `${e.name}: ${e.message}`, true); }
  // RTCPeerConnection: a loopback pair exchanging a data-channel message.
  try {
    // Trickle ICE with candidates buffered until the remote description is set.
    const a = new RTCPeerConnection(), b = new RTCPeerConnection();
    const wire = (from, to) => {
      const queue = [];
      from.onicecandidate = e => { if (!e.candidate) return; if (to.remoteDescription) to.addIceCandidate(e.candidate).catch(() => {}); else queue.push(e.candidate); };
      return () => Promise.all(queue.splice(0).map(c => to.addIceCandidate(c).catch(() => {})));
    };
    const flushToB = wire(a, b), flushToA = wire(b, a);
    const got = new Promise(res => { b.ondatachannel = ev => { ev.channel.onmessage = m => res(m.data); }; });
    const remoteTracks = [];
    b.ontrack = ev => remoteTracks.push(ev.track.kind);
    const ch = a.createDataChannel('probe');
    ch.onopen = () => ch.send('hello');
    if (stream) stream.getTracks().forEach(t => a.addTrack(t, stream));
    await a.setLocalDescription(await a.createOffer());
    await b.setRemoteDescription(a.localDescription); await flushToB();
    await b.setLocalDescription(await b.createAnswer());
    await a.setRemoteDescription(b.localDescription); await flushToA();
    const msg = await withTimeout(got, 15000, `data channel (ice ${a.iceConnectionState}/${b.iceConnectionState}, gathering ${a.iceGatheringState})`);
    rec('rtcPeerConnection', msg === 'hello', `loopback data channel delivered "${msg}"; remote tracks: ${remoteTracks.join(',') || 'none'} (${streamSource})`, true);
    a.close(); b.close();
  } catch (e) { rec('rtcPeerConnection', false, `${e.name}: ${e.message}`, true); }
  if (stream) { stream.getTracks().forEach(t => t.stop()); stream._cleanup?.(); }
}
run().catch(e => rec('probe', false, e.message, true)).finally(() => {
  window.webkit.messageHandlers.probe.postMessage(JSON.stringify({checks, userAgent: navigator.userAgent, origin: location.origin}));
});
</script></body></html>
"""#
}
