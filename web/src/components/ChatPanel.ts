import { chatTime, mergeChat } from '../lib/callstate';
import { linkifySegments } from '../lib/linkify';
import { ChatMaxChars, type ChatMessage } from '../lib/protocol';

// In-call chat. Message text is untrusted: it is only ever written with
// textContent, and only http(s) URLs become links (new tab, no opener/referrer).
export class ChatPanel {
  readonly element: HTMLElement;
  private readonly log: HTMLElement;
  private readonly empty: HTMLElement;
  private readonly input: HTMLTextAreaElement;
  private readonly counter: HTMLElement;
  private readonly sendBtn: HTMLButtonElement;
  private messages: ChatMessage[] = [];
  private unread = 0;

  constructor(
    private readonly opts: {
      send: (text: string) => void;
      selfId: () => string;
      onUnreadChange: (count: number) => void;
      onClose: () => void;
    },
  ) {
    const el = document.createElement('section');
    el.className = 'call-panel-body chat-panel';
    el.setAttribute('aria-labelledby', 'chat-panel-title');
    el.innerHTML = `
      <div class="call-panel-head">
        <h2 id="chat-panel-title">In-call messages</h2>
        <button type="button" class="icon-button" data-close aria-label="Close messages">×</button>
      </div>
      <p class="call-panel-note">Everyone in the call sees these messages, including people who join later. They aren’t part of the recording and are gone when the meeting ends.</p>
      <div class="chat-log" role="log" aria-live="polite" aria-relevant="additions" tabindex="0" aria-label="Messages"></div>
      <p class="chat-empty">No messages yet.</p>
      <form class="chat-compose">
        <label class="visually-hidden" for="chat-input">Send a message to everyone</label>
        <textarea id="chat-input" rows="2" maxlength="${ChatMaxChars}" placeholder="Send a message to everyone" autocomplete="off"></textarea>
        <div class="chat-compose-row"><span class="chat-counter" aria-live="polite"></span><button type="submit" class="btn btn-primary chat-send">Send</button></div>
      </form>`;
    this.element = el;
    this.log = el.querySelector('.chat-log')!;
    this.empty = el.querySelector('.chat-empty')!;
    this.input = el.querySelector('textarea')!;
    this.counter = el.querySelector('.chat-counter')!;
    this.sendBtn = el.querySelector('.chat-send')!;
    el.querySelector<HTMLButtonElement>('[data-close]')!.onclick = () => this.opts.onClose();
    el.querySelector('form')!.addEventListener('submit', e => { e.preventDefault(); this.submit(); });
    this.input.addEventListener('keydown', e => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); this.submit(); }
    });
    this.input.addEventListener('input', () => this.updateCounter());
    this.updateCounter();
  }

  get isVisible(): boolean {
    return !this.element.hidden && !!this.element.closest('.call-panel.open');
  }

  focusInput(): void {
    this.input.focus();
  }

  markRead(): void {
    if (this.unread === 0) return;
    this.unread = 0;
    this.opts.onUnreadChange(0);
  }

  /** History replay on admission/resume: merge, never duplicate. */
  setHistory(history: ChatMessage[]): void {
    this.messages = mergeChat(this.messages, history);
    this.render();
  }

  add(msg: ChatMessage): void {
    if (this.messages.some(m => m.id === msg.id)) return;
    this.messages = mergeChat(this.messages, [msg]);
    const nearBottom = this.log.scrollHeight - this.log.scrollTop - this.log.clientHeight < 60;
    const at = this.messages.findIndex(m => m.id === msg.id);
    if (at === this.messages.length - 1 && this.log.childElementCount === at) {
      // Append only, so the live log announces just the new line.
      this.log.append(this.item(msg, this.messages[at - 1]));
      this.empty.hidden = true;
    } else {
      this.render();
    }
    if (nearBottom || msg.participantId === this.opts.selfId()) this.log.scrollTop = this.log.scrollHeight;
    if (msg.participantId !== this.opts.selfId() && !this.isVisible) {
      this.unread++;
      this.opts.onUnreadChange(this.unread);
    }
  }

  private submit(): void {
    const text = this.input.value.trim();
    if (!text) return;
    if ([...text].length > ChatMaxChars) return;
    this.opts.send(text);
    this.input.value = '';
    this.updateCounter();
  }

  private updateCounter(): void {
    const n = [...this.input.value].length;
    this.counter.textContent = n > ChatMaxChars - 200 ? `${ChatMaxChars - n} characters left` : '';
    this.sendBtn.disabled = this.input.value.trim() === '';
  }

  private render(): void {
    const frag = document.createDocumentFragment();
    let prev: ChatMessage | undefined;
    for (const msg of this.messages) {
      frag.append(this.item(msg, prev));
      prev = msg;
    }
    this.log.replaceChildren(frag);
    this.empty.hidden = this.messages.length > 0;
  }

  private item(msg: ChatMessage, prev: ChatMessage | undefined): HTMLElement {
    const self = this.opts.selfId();
    const grouped = prev && prev.participantId === msg.participantId && msg.ts - prev.ts < 120_000;
    const item = document.createElement('article');
    item.className = 'chat-msg' + (msg.participantId === self ? ' chat-msg-self' : '') + (grouped ? ' chat-msg-grouped' : '');
    item.dataset.messageId = msg.id;
    if (!grouped) {
      const meta = document.createElement('header');
      meta.className = 'chat-meta';
      const who = document.createElement('strong');
      who.textContent = msg.participantId === self ? 'You' : msg.name || 'Participant';
      const when = document.createElement('time');
      when.dateTime = new Date(msg.ts).toISOString();
      when.textContent = chatTime(msg.ts);
      meta.append(who);
      if (msg.role === 'host') {
        const role = document.createElement('span');
        role.className = 'chat-role';
        role.textContent = 'Host';
        meta.append(role);
      }
      meta.append(when);
      item.append(meta);
    }
    const body = document.createElement('p');
    body.className = 'chat-text';
    for (const seg of linkifySegments(msg.text)) {
      if (seg.kind === 'link') {
        const a = document.createElement('a');
        a.href = seg.href;
        a.textContent = seg.text;
        a.target = '_blank';
        a.rel = 'noopener noreferrer';
        a.referrerPolicy = 'no-referrer';
        body.append(a);
      } else {
        body.append(document.createTextNode(seg.text));
      }
    }
    item.append(body);
    return item;
  }
}
