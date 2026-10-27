import { AllowedReactions, type ReactionEmoji } from '../lib/protocol';
import { ICONS } from './icons';

const REACTION_NAMES: Record<ReactionEmoji, string> = {
  '👍': 'Thumbs up',
  '❤️': 'Heart',
  '😂': 'Laughing',
  '😮': 'Surprised',
  '👏': 'Applause',
  '🎉': 'Celebrate',
};

// Emoji reaction picker (a small popover off the dock) and the floating
// animation shown on the sender's tile. Reactions are ephemeral: nothing is
// stored, and the host rate-limits them.
export class Reactions {
  readonly button: HTMLButtonElement;
  readonly popover: HTMLElement;

  constructor(private readonly opts: { send: (emoji: ReactionEmoji) => void }) {
    const button = document.createElement('button');
    button.type = 'button';
    button.id = 'react-btn';
    button.className = 'btn btn-secondary collab-btn';
    button.setAttribute('aria-haspopup', 'true');
    button.setAttribute('aria-expanded', 'false');
    button.setAttribute('aria-controls', 'reaction-picker');
    button.classList.add('collab-compact');
    button.setAttribute('aria-label', 'Send a reaction');
    button.title = 'React';
    button.innerHTML = `<span class="collab-icon" aria-hidden="true">${ICONS.react}</span><span class="collab-label">React</span>`;
    this.button = button;

    const pop = document.createElement('div');
    pop.id = 'reaction-picker';
    pop.className = 'reaction-picker';
    pop.setAttribute('role', 'group');
    pop.setAttribute('aria-label', 'Send a reaction');
    pop.hidden = true;
    for (const emoji of AllowedReactions) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'reaction-choice';
      b.textContent = emoji;
      b.dataset.emoji = emoji;
      b.setAttribute('aria-label', REACTION_NAMES[emoji]);
      b.title = REACTION_NAMES[emoji];
      b.onclick = () => { this.opts.send(emoji); };
      pop.append(b);
    }
    this.popover = pop;

    button.onclick = () => this.toggle();
    pop.addEventListener('keydown', e => {
      const choices = [...pop.querySelectorAll<HTMLButtonElement>('.reaction-choice')];
      const i = choices.indexOf(document.activeElement as HTMLButtonElement);
      if (e.key === 'Escape') { e.stopPropagation(); this.close(true); }
      else if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
        e.preventDefault();
        choices[(i + (e.key === 'ArrowRight' ? 1 : choices.length - 1)) % choices.length]?.focus();
      }
    });
    document.addEventListener('pointerdown', e => {
      if (!pop.hidden && !pop.contains(e.target as Node) && !button.contains(e.target as Node)) this.close(false);
    });
  }

  toggle(): void {
    if (this.popover.hidden) this.open();
    else this.close(true);
  }

  open(): void {
    this.popover.hidden = false;
    this.button.setAttribute('aria-expanded', 'true');
    this.popover.querySelector<HTMLButtonElement>('.reaction-choice')?.focus();
  }

  close(restoreFocus: boolean): void {
    if (this.popover.hidden) return;
    this.popover.hidden = true;
    this.button.setAttribute('aria-expanded', 'false');
    if (restoreFocus) this.button.focus();
  }

  /** Float an emoji up over a tile. Decorative: hidden from assistive tech. */
  static float(tile: Element | null, emoji: string, name: string): void {
    if (!tile) return;
    let layer = tile.querySelector<HTMLElement>('.reaction-layer');
    if (!layer) {
      layer = document.createElement('div');
      layer.className = 'reaction-layer';
      layer.setAttribute('aria-hidden', 'true');
      tile.append(layer);
    }
    const bubble = document.createElement('div');
    bubble.className = 'reaction-float';
    bubble.style.setProperty('--drift', `${Math.round(Math.random() * 40 - 20)}px`);
    const glyph = document.createElement('span');
    glyph.className = 'reaction-glyph';
    glyph.textContent = emoji;
    const label = document.createElement('span');
    label.className = 'reaction-name';
    label.textContent = name;
    bubble.append(glyph, label);
    layer.append(bubble);
    while (layer.childElementCount > 8) layer.firstElementChild?.remove();
    setTimeout(() => bubble.remove(), 3200);
  }
}
