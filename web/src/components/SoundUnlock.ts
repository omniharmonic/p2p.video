// Autoplay with sound [SL-P1-12]. Safari (always on iPhone, sometimes on the
// Mac) refuses to start unmuted media that wasn't started by a user gesture,
// and an <video autoplay> that loses that race stays paused: no picture and
// no sound from that participant. Every remote element goes through
// ensurePlaying(): if play() is refused, the picture starts muted right away
// and a "Turn on call sound" button appears; the next tap or key press (any
// gesture on the page) unmutes and resumes everything that was held back.
export class SoundUnlock {
  private readonly held = new Set<HTMLMediaElement>();
  private button: HTMLButtonElement | null = null;

  constructor(private readonly root: HTMLElement = document.body) {
    const onGesture = () => { if (this.held.size) this.unlock(); };
    // Capture phase: runs inside the gesture, before any handler stops it.
    for (const type of ['pointerdown', 'touchend', 'keydown']) document.addEventListener(type, onGesture, true);
  }

  /** Remote media still waiting for a gesture to play with sound. */
  get waiting(): number { return this.held.size; }

  /** Starts a remote element, falling back to muted playback plus the unlock button. */
  ensurePlaying(el: HTMLMediaElement): void {
    if (!el.paused && !el.muted) return;
    const attempt = el.play?.();
    if (!attempt || typeof attempt.catch !== 'function') return;
    attempt.catch((error: unknown) => {
      if ((error as { name?: string })?.name !== 'NotAllowedError' || !el.isConnected) return;
      el.muted = true;
      this.held.add(el);
      void el.play().catch(() => { /* Even muted playback refused: the tap starts it. */ });
      this.show();
    });
  }

  /** Unmutes and plays everything held back. Must run inside a user gesture. */
  unlock(): void {
    for (const el of this.held) {
      if (!el.isConnected) continue;
      el.muted = false;
      void el.play().catch(() => { el.muted = true; });
    }
    this.held.clear();
    this.button?.remove();
    this.button = null;
  }

  private show(): void {
    if (this.button) return;
    const button = document.createElement('button');
    button.type = 'button';
    button.id = 'sound-unlock-btn';
    button.className = 'btn btn-primary sound-unlock';
    button.textContent = 'Turn on call sound';
    button.setAttribute('aria-live', 'polite');
    button.onclick = () => this.unlock();
    // Fixed above the dock; inline styles keep it independent of the call CSS.
    Object.assign(button.style, { position: 'fixed', left: '50%', transform: 'translateX(-50%)', bottom: 'calc(var(--dock-h, 80px) + 16px)', zIndex: '1000' });
    this.root.append(button);
    this.button = button;
  }
}

export const soundUnlock = new SoundUnlock();
