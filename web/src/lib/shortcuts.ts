// Meet-compatible call shortcuts as a pure function, so matching can be unit
// tested under node. Self-contained: no imports.
//   ⌘/Ctrl+D  toggle microphone      ⌘/Ctrl+E  toggle camera
//   Ctrl+⌘+H  raise/lower hand (Ctrl+Alt+H off macOS)
//   c         show/hide live captions [SL-P4-2]
//   ?         keyboard shortcut help

export type ShortcutAction = 'toggle-mic' | 'toggle-cam' | 'raise-hand' | 'toggle-captions' | 'help';

export interface KeyLike {
  key: string;
  code?: string;
  metaKey: boolean;
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
  repeat?: boolean;
}

export interface TargetLike {
  tagName?: string;
  isContentEditable?: boolean;
  type?: string;
}

const NON_TEXT_INPUTS = new Set(['button', 'checkbox', 'radio', 'range', 'color', 'file', 'submit', 'reset', 'image']);

/** True when keys typed at this element belong to the user's text, not to us. */
export function isTypingTarget(target: TargetLike | null | undefined): boolean {
  if (!target) return false;
  if (target.isContentEditable) return true;
  const tag = (target.tagName || '').toUpperCase();
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  if (tag === 'INPUT') return !NON_TEXT_INPUTS.has((target.type || 'text').toLowerCase());
  return false;
}

function letter(e: KeyLike): string {
  if (e.code && /^Key[A-Z]$/.test(e.code)) return e.code.slice(3).toLowerCase();
  return e.key.length === 1 ? e.key.toLowerCase() : '';
}

export function matchShortcut(e: KeyLike, isMac: boolean, target?: TargetLike | null): ShortcutAction | null {
  if (e.repeat) return null;
  if (isTypingTarget(target)) return null;
  const k = letter(e);
  const primary = isMac ? e.metaKey && !e.ctrlKey : e.ctrlKey && !e.metaKey;
  if (primary && !e.altKey && !e.shiftKey) {
    if (k === 'd') return 'toggle-mic';
    if (k === 'e') return 'toggle-cam';
  }
  const handChord = isMac ? e.metaKey && e.ctrlKey && !e.altKey : e.ctrlKey && e.altKey && !e.metaKey;
  if (handChord && !e.shiftKey && k === 'h') return 'raise-hand';
  if (k === 'c' && !e.metaKey && !e.ctrlKey && !e.altKey && !e.shiftKey) return 'toggle-captions';
  if (e.key === '?' && !e.metaKey && !e.ctrlKey && !e.altKey) return 'help';
  return null;
}

/** Human-readable bindings for the help dialog. */
export function shortcutLabels(isMac: boolean): Array<{ action: ShortcutAction; keys: string[]; label: string }> {
  const mod = isMac ? '⌘' : 'Ctrl';
  return [
    { action: 'toggle-mic', keys: [mod, 'D'], label: 'Mute or unmute microphone' },
    { action: 'toggle-cam', keys: [mod, 'E'], label: 'Turn camera on or off' },
    { action: 'raise-hand', keys: isMac ? ['Ctrl', '⌘', 'H'] : ['Ctrl', 'Alt', 'H'], label: 'Raise or lower hand' },
    { action: 'toggle-captions', keys: ['C'], label: 'Show or hide captions' },
    { action: 'help', keys: ['?'], label: 'Show keyboard shortcuts' },
  ];
}
