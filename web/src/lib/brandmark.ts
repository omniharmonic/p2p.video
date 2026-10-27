// The p2p.video brand mark, wordmark and source-code offer [SL-REL-1].
//
// Mark: two linked rings (host and guest), the app icon's motif, with the
// red recording lens where they overlap. Wordmark: the domain, lowercase,
// in the display face, with ".video" one step quieter. index.html and
// record.html carry static copies of the same markup (they render before
// any script); keep them in step with these strings.
//
// AGPL-3.0 §13: people who use the app over the network (guests included)
// are offered its source. `annotateSourceLinks` adds the build this host
// runs (from /health) to every `[data-source-link]` so the offer names it.

export const SOURCE_URL = 'https://github.com/omniharmonic/p2p.video';

export const BRAND_MARK_SVG = '<svg viewBox="0 0 28 28" fill="none" aria-hidden="true">'
  + '<circle cx="10.5" cy="14" r="6.5" stroke="currentColor" stroke-width="2"/>'
  + '<circle cx="17.5" cy="14" r="6.5" stroke="currentColor" stroke-width="2"/>'
  + '<path class="brand-lens" d="M14 8.52A6.5 6.5 0 0 1 14 19.48A6.5 6.5 0 0 1 14 8.52Z"/></svg>';

export const WORDMARK_HTML = '<span class="wordmark">p2p<span class="wordmark-tld">.video</span></span>';

/** A small "Source code" link; `annotateSourceLinks` fills in the build. */
export function sourceLinkHTML(label = 'Source code'): string {
  return `<a class="source-link" data-source-link href="${SOURCE_URL}" target="_blank" rel="noopener noreferrer">${label}</a>`;
}

/** The title shown on source links, naming this host's build when known. */
export function sourceTitle(buildId: string | null | undefined): string {
  const base = 'p2p.video is free software under the GNU AGPL v3. Get the source code.';
  const id = (buildId ?? '').trim();
  return /^[\w.:-]{1,128}$/.test(id) ? `${base} This host runs build ${id}.` : base;
}

let build: Promise<string | null> | null = null;

/** Sets the title (with the host's build ID) on every source link under `root`. */
export async function annotateSourceLinks(root: ParentNode = document): Promise<void> {
  build ??= fetch('/health', { cache: 'no-store' })
    .then(r => (r.ok ? r.json() : null))
    .then(h => (h && typeof h.buildId === 'string' ? h.buildId : null))
    .catch(() => null);
  const id = await build;
  root.querySelectorAll<HTMLAnchorElement>('a[data-source-link]').forEach(a => { a.title = sourceTitle(id); });
}
