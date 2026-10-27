// Live-captions tap [SL-P4-2]. Runs on the audio render thread of the HOST's
// page only. Input is call audio (remote WebRTC tracks and the host's own mic)
// for captions; it is never recorded, uploaded or written into a take.
// Output: 100 ms frames of 16 kHz mono Int16, posted to the main thread.
const TARGET_RATE = 16000;
const FRAME = TARGET_RATE / 10;

class StudioLinkCaptionTap extends AudioWorkletProcessor {
  constructor() {
    super();
    this.ratio = sampleRate / TARGET_RATE; // input samples per output sample
    this.acc = 0;
    this.sum = 0;
    this.count = 0;
    this.out = new Int16Array(FRAME);
    this.pos = 0;
    this.active = true;
    this.port.onmessage = event => { if (event.data === 'stop') this.active = false; };
  }

  process(inputs) {
    if (!this.active) return false;
    const input = inputs[0];
    if (!input || !input.length || !input[0]) return true;
    const a = input[0];
    const b = input.length > 1 ? input[1] : null;
    for (let i = 0; i < a.length; i++) {
      // Average the input samples that fall into each output sample: a box
      // low-pass that is plenty for speech recognition.
      this.sum += b ? (a[i] + b[i]) * 0.5 : a[i];
      this.count++;
      this.acc += 1;
      if (this.acc >= this.ratio) {
        this.acc -= this.ratio;
        const v = Math.max(-1, Math.min(1, this.sum / this.count));
        this.out[this.pos++] = v < 0 ? Math.round(v * 32768) : Math.round(v * 32767);
        this.sum = 0;
        this.count = 0;
        if (this.pos === FRAME) {
          const frame = this.out;
          this.out = new Int16Array(FRAME);
          this.pos = 0;
          this.port.postMessage(frame, [frame.buffer]);
        }
      }
    }
    return true;
  }
}

registerProcessor('studiolink-caption-tap', StudioLinkCaptionTap);
