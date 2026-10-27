// Split untrusted chat text into plain-text and link segments. Only absolute
// http(s) URLs become links; everything else stays text and must be rendered
// with textContent. No HTML is produced here. Self-contained (no imports) so
// it can be unit-tested directly under node.

export type ChatSegment = { kind: 'text'; text: string } | { kind: 'link'; text: string; href: string };

const URL_PATTERN = /\bhttps?:\/\/[^\s<>"'`]+/gi;
// Sentence punctuation that usually follows a URL rather than belonging to it.
const TRAILING = /[.,;:!?)\]}]+$/;

export function safeHttpUrl(candidate: string): string | null {
  let url: URL;
  try {
    url = new URL(candidate);
  } catch {
    return null;
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;
  if (!url.hostname) return null;
  return url.href;
}

export function linkifySegments(text: string): ChatSegment[] {
  const out: ChatSegment[] = [];
  let last = 0;
  const pushText = (value: string) => {
    if (!value) return;
    const prev = out[out.length - 1];
    if (prev?.kind === 'text') prev.text += value;
    else out.push({ kind: 'text', text: value });
  };
  for (const match of text.matchAll(URL_PATTERN)) {
    const start = match.index ?? 0;
    let trail = match[0].match(TRAILING)?.[0] ?? '';
    let raw = match[0].slice(0, match[0].length - trail.length);
    // Keep a closing parenthesis that balances one inside the URL, as in
    // https://en.wikipedia.org/wiki/Mix_(audio).
    const count = (s: string, ch: string) => s.split(ch).length - 1;
    if (trail.startsWith(')') && count(raw, '(') > count(raw, ')')) {
      raw += ')';
      trail = trail.slice(1);
    }
    const href = safeHttpUrl(raw);
    pushText(text.slice(last, start));
    if (href) out.push({ kind: 'link', text: raw, href });
    else pushText(raw);
    pushText(trail);
    last = start + match[0].length;
  }
  pushText(text.slice(last));
  return out;
}
