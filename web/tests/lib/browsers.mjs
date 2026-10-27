// Cross-browser launch helpers [SL-P1-12]: Chromium, Firefox and WebKit with
// fake media, headless on macOS.
//
// - Chromium: --use-fake-device-for-media-stream (+ fake UI) — the y4m-less
//   built-in pattern camera and beep microphone.
// - Firefox: media.navigator.streams.fake + media.navigator.permission.disabled
//   — Gecko's built-in fake camera (moving pattern) and tone microphone, no
//   prompt. Real getUserMedia, real devices list.
// - WebKit: Playwright's WebKit build enumerates mock capture devices, but on
//   macOS getUserMedia never resolves (no permission is grantable: Playwright's
//   WebKit grantPermissions knows no "camera"/"microphone", and the embedder's
//   media-capture prompt is never answered, headless or headed). So WebKit
//   pages get a page-level synthetic capture instead: getUserMedia returns a
//   canvas.captureStream() camera (moving frame counter) and an oscillator
//   through createMediaStreamDestination() microphone. Everything downstream —
//   MediaRecorder, WebRTC encode/decode, OPFS, AudioWorklet, layout — is real
//   WebKit; only the capture source is synthetic.

/** Firefox prefs: fake devices, no prompt, autoplay allowed (as after a click). */
export const firefoxPrefs = {
  'media.navigator.streams.fake': true,
  'media.navigator.permission.disabled': true,
  'media.autoplay.default': 0,
  'media.autoplay.block-webaudio': false,
  'media.getusermedia.screensharing.enabled': true,
  'dom.webnotifications.enabled': false,
};

/**
 * Init script for WebKit pages: synthetic camera + microphone behind
 * getUserMedia/enumerateDevices. Runs in the page before any app code.
 */
export function webkitSyntheticCapture() {
  const md = navigator.mediaDevices;
  if (!md || window.__syntheticCapture) return;
  window.__syntheticCapture = true;
  let ctx = null;
  const audioContext = () => {
    if (!ctx) {
      ctx = new AudioContext({ sampleRate: 48000 });
      const resume = () => { if (ctx.state !== 'running') void ctx.resume(); };
      addEventListener('pointerdown', resume, true);
      addEventListener('keydown', resume, true);
    }
    void ctx.resume().catch(() => {});
    return ctx;
  };
  const camera = (width, height) => {
    const canvas = document.createElement('canvas');
    canvas.width = width; canvas.height = height;
    const g = canvas.getContext('2d');
    let frame = 0;
    const draw = () => {
      frame++;
      g.fillStyle = `hsl(${(frame * 4) % 360} 55% 35%)`;
      g.fillRect(0, 0, width, height);
      g.fillStyle = '#fff';
      g.fillRect((frame * 8) % width, height / 2 - 40, 80, 80);
      g.font = '48px -apple-system, sans-serif';
      g.fillText(`WebKit synthetic camera · ${frame}`, 40, 80);
    };
    draw();
    const timer = setInterval(draw, 1000 / 30);
    const track = canvas.captureStream(30).getVideoTracks()[0];
    track.addEventListener('ended', () => clearInterval(timer));
    const stop = track.stop.bind(track);
    track.stop = () => { clearInterval(timer); stop(); };
    return track;
  };
  const microphone = () => {
    const c = audioContext();
    const osc = c.createOscillator();
    osc.frequency.value = 440;
    const gain = c.createGain();
    gain.gain.value = 0.2;
    const dest = c.createMediaStreamDestination();
    osc.connect(gain); gain.connect(dest); osc.start();
    const track = dest.stream.getAudioTracks()[0];
    const stop = track.stop.bind(track);
    track.stop = () => { try { osc.stop(); } catch { /* already stopped */ } stop(); };
    return track;
  };
  // Synthetic inputs; outputs are whatever this WebKit itself lists, so the
  // speaker-choice detection still reflects the real engine.
  const inputs = [
    { kind: 'videoinput', deviceId: 'synthetic-camera', label: 'Synthetic camera (WebKit test)', groupId: 'synthetic' },
    { kind: 'audioinput', deviceId: 'synthetic-microphone', label: 'Synthetic microphone (WebKit test)', groupId: 'synthetic' },
  ].map(d => ({ ...d, toJSON() { return { ...d }; } }));
  const enumerate = md.enumerateDevices.bind(md);
  md.enumerateDevices = async () => [...inputs, ...(await enumerate()).filter(d => d.kind === 'audiooutput')];
  md.getUserMedia = async constraints => {
    const tracks = [];
    if (constraints?.video) {
      const v = typeof constraints.video === 'object' ? constraints.video : {};
      const pick = (c, fallback) => typeof c === 'number' ? c : (c?.exact ?? c?.ideal ?? fallback);
      tracks.push(camera(Math.min(pick(v.width, 1280), 1280), Math.min(pick(v.height, 720), 720)));
    }
    if (constraints?.audio) tracks.push(microphone());
    if (!tracks.length) throw new TypeError('getUserMedia needs audio or video');
    return new MediaStream(tracks);
  };
}

/**
 * Launches `name` ('chromium' | 'firefox' | 'webkit') headless with fake media.
 * Chromium extra args (e.g. --mute-audio) can be passed.
 */
export async function launchBrowser(pw, name, { chromiumArgs = [] } = {}) {
  if (name === 'chromium') {
    return pw.chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH, args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required', ...chromiumArgs] });
  }
  if (name === 'firefox') return pw.firefox.launch({ headless: true, executablePath: process.env.FIREFOX_PATH, firefoxUserPrefs: firefoxPrefs });
  if (name === 'webkit') return pw.webkit.launch({ headless: true, executablePath: process.env.WEBKIT_PATH });
  throw new Error(`unknown browser ${name}`);
}

/** Context options per engine: Chromium grants camera/mic; the others need none. */
export function contextOptions(name, extra = {}) {
  return name === 'chromium' ? { permissions: ['camera', 'microphone'], ...extra } : { ...extra };
}

/** Per-context init: RTCPeerConnection tracking (+ WebKit synthetic capture). */
export async function prepareContext(context, name) {
  // window.__frames(video): frames presented so far. getVideoPlaybackQuality()
  // stays 0 for MediaStream playback in WebKit, so a requestVideoFrameCallback
  // counter (all three engines) is used, whichever is larger.
  await context.addInitScript(() => {
    const counted = new WeakMap();
    window.__frames = video => {
      if (!video) return 0;
      if (!counted.has(video)) {
        counted.set(video, 0);
        if (typeof video.requestVideoFrameCallback === 'function') {
          const tick = () => { counted.set(video, counted.get(video) + 1); video.requestVideoFrameCallback(tick); };
          video.requestVideoFrameCallback(tick);
        }
      }
      return Math.max(counted.get(video), video.getVideoPlaybackQuality?.().totalVideoFrames || 0);
    };
  });
  await context.addInitScript(() => {
    const Original = window.RTCPeerConnection; window.__pcs = [];
    window.RTCPeerConnection = class extends Original { constructor(...args) { super(...args); window.__pcs.push(this); } };
  });
  if (name === 'webkit') await context.addInitScript(webkitSyntheticCapture);
}

/** Close a browser without hanging the suite (Firefox can stall on exit with live media). */
export async function closeBrowser(browser) {
  if (!browser) return;
  await Promise.race([browser.close().catch(() => {}), new Promise(r => setTimeout(r, 5000))]);
}

export const engineLabel = name => ({ chromium: 'Chromium', firefox: 'Firefox', webkit: 'WebKit' })[name] || name;
