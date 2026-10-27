// The one icon set for p2p.video: 24-unit line icons, round caps and joins,
// stroke = currentColor. Rendered inline (no icon fonts or CDNs). Static,
// trusted markup. Size comes from CSS (.collab-icon svg / .ui-icon svg), so
// the width/height attributes are only a fallback. See docs/design/design-system.md.
const svg = (d: string) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" focusable="false">${d}</svg>`;
export const ICONS = {
  hand: svg('<path d="M8 12V5.5a1.5 1.5 0 0 1 3 0V11"/><path d="M11 10V4a1.5 1.5 0 0 1 3 0v6"/><path d="M14 10V5.5a1.5 1.5 0 0 1 3 0V13"/><path d="M8 12.5 6.6 10.8a1.6 1.6 0 0 0-2.5 2l3 4.4A6 6 0 0 0 12 20h.5a4.5 4.5 0 0 0 4.5-4.5V13"/>'),
  react: svg('<circle cx="12" cy="12" r="8.5"/><path d="M8.5 14.2a4.2 4.2 0 0 0 7 0"/><path d="M9.2 9.6h.01M14.8 9.6h.01" stroke-width="2.4"/>'),
  chat: svg('<path d="M4.5 5.5h15v10h-9l-4.5 3.5v-3.5h-1.5z"/><path d="M8.5 9.5h7M8.5 12.5h4"/>'),
  people: svg('<circle cx="9" cy="8.5" r="3"/><path d="M3.5 19a5.5 5.5 0 0 1 11 0"/><circle cx="16.5" cy="9.5" r="2.3"/><path d="M15.5 14.1a4.6 4.6 0 0 1 5 4.9"/>'),
  keys: svg('<rect x="3" y="6.5" width="18" height="11" rx="1.5"/><path d="M7 10h.01M10.5 10h.01M14 10h.01M17.5 10h.01M8 14h8" stroke-width="1.9"/>'),
  screen: svg('<rect x="3" y="4.5" width="18" height="12" rx="1.5"/><path d="M9 20h6M12 16.5V20"/><path d="M12 13.5V8M9.5 10.3 12 7.8l2.5 2.5"/>'),
  sliders: svg('<path d="M5 6h9M18 6h1M5 12h3M12 12h7M5 18h11M20 18h-1"/><circle cx="16" cy="6" r="2"/><circle cx="10" cy="12" r="2"/><circle cx="18" cy="18" r="2"/>'),
  cc: svg('<rect x="3" y="5.5" width="18" height="13" rx="2"/><path d="M10.5 10.2a2.4 2.4 0 1 0 0 3.6M16.5 10.2a2.4 2.4 0 1 0 0 3.6"/>'),
  mic: svg('<rect x="9" y="3.5" width="6" height="11" rx="3"/><path d="M5.5 11.5a6.5 6.5 0 0 0 13 0M12 18v2.5"/>'),
  micOff: svg('<path d="M15 10V6.5a3 3 0 0 0-5.6-1.5M9 9v2.5a3 3 0 0 0 4.6 2.5"/><path d="M5.5 11.5a6.5 6.5 0 0 0 10.6 5M18.5 11.5a6.4 6.4 0 0 1-.5 2.4M12 18v2.5M4 4l16 16"/>'),
  cam: svg('<rect x="3" y="6.5" width="12.5" height="11" rx="2"/><path d="m15.5 10.5 5-3v9l-5-3z"/>'),
  camOff: svg('<path d="M9 6.5h4.5a2 2 0 0 1 2 2v3M15.5 15.5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-7a2 2 0 0 1 1.5-2"/><path d="m15.5 10.5 5-3v9l-3.2-1.9M4 4l16 16"/>'),
  more: svg('<circle cx="12" cy="5.5" r="1.2" fill="currentColor"/><circle cx="12" cy="12" r="1.2" fill="currentColor"/><circle cx="12" cy="18.5" r="1.2" fill="currentColor"/>'),
  end: svg('<path d="M3.5 13.8c4.7-4.4 12.3-4.4 17 0l-2 2.6-3.6-1.4v-2.6a10.4 10.4 0 0 0-5.8 0V15l-3.6 1.4z" fill="currentColor"/>'),
  record: svg('<circle cx="12" cy="12" r="8"/><circle cx="12" cy="12" r="3.6" fill="currentColor" stroke="none"/>'),
  stop: svg('<rect x="6.5" y="6.5" width="11" height="11" rx="2" fill="currentColor" stroke="none"/>'),
  health: svg('<path d="M3 12h4l2-5 4 10 2-5h6"/>'),
  layout: svg('<rect x="3.5" y="4.5" width="17" height="15" rx="2"/><path d="M12 4.5v15M3.5 12h8.5"/>'),
  settings: svg('<circle cx="12" cy="12" r="3"/><path d="M19.4 13.5a7.6 7.6 0 0 0 0-3l2-1.6-2-3.4-2.4 1a7.5 7.5 0 0 0-2.6-1.5L14 2.5h-4l-.4 2.5A7.5 7.5 0 0 0 7 6.5l-2.4-1-2 3.4 2 1.6a7.6 7.6 0 0 0 0 3l-2 1.6 2 3.4 2.4-1a7.5 7.5 0 0 0 2.6 1.5l.4 2.5h4l.4-2.5a7.5 7.5 0 0 0 2.6-1.5l2.4 1 2-3.4z"/>'),
  transfers: svg('<path d="M12 4v11M7.5 10.5 12 15l4.5-4.5"/><path d="M4.5 16.5v2a1.5 1.5 0 0 0 1.5 1.5h12a1.5 1.5 0 0 0 1.5-1.5v-2"/>'),
  live: svg('<circle cx="12" cy="12" r="2.2"/><path d="M7.8 7.8a6 6 0 0 0 0 8.4M16.2 7.8a6 6 0 0 1 0 8.4M5 5a10 10 0 0 0 0 14M19 5a10 10 0 0 1 0 14"/>'),
  chirp: svg('<path d="M4 9.5h3.5L12 5.5v13l-4.5-4H4z"/><path d="M15.5 9a4 4 0 0 1 0 6M18 6.5a7.5 7.5 0 0 1 0 11"/>'),
  export: svg('<path d="M12 15V4M7.5 8.5 12 4l4.5 4.5"/><path d="M4.5 13.5v5a1.5 1.5 0 0 0 1.5 1.5h12a1.5 1.5 0 0 0 1.5-1.5v-5"/>'),
  guide: svg('<circle cx="12" cy="12" r="8.5"/><path d="M9.6 9.4a2.5 2.5 0 1 1 3.4 2.4c-.6.3-1 .8-1 1.5v.4M12 16.6h.01" stroke-width="1.9"/>'),
  invite: svg('<circle cx="10" cy="8.5" r="3.2"/><path d="M4 19a6 6 0 0 1 12 0M18.5 8v6M15.5 11h6"/>'),
  close: svg('<path d="M6 6l12 12M18 6 6 18"/>'),
  effects: svg('<circle cx="12" cy="9.5" r="3"/><path d="M6.5 19.5c.8-3 3-4.5 5.5-4.5s4.7 1.5 5.5 4.5"/><path d="M3.5 6.5v.01M3.5 12v.01M3.5 17.5v.01M20.5 6.5v.01M20.5 12v.01M20.5 17.5v.01" stroke-width="2.4"/>'),
  phone: svg('<rect x="7" y="3" width="10" height="18" rx="2.2"/><path d="M11 17.5h2"/>'),
  // Teleprompter: a script page under a camera lens [SL-P5-1].
  prompter: svg('<circle cx="12" cy="4.5" r="1.6"/><rect x="5" y="8" width="14" height="12" rx="1.5"/><path d="M8.5 11.5h7M8.5 14.5h7M8.5 17.2h4"/>'),
  // Branding: a lower-third bar with its accent strip [SL-P5-2].
  brand: svg('<rect x="3.5" y="5" width="17" height="14" rx="1.5"/><path d="M6.5 12.5v4"/><path d="M9 13.5h7M9 16h4.5"/>'),
  // In-call chrome [SL-UX-2]: details, copy link, the Studio panel and split-button carets.
  info: svg('<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5.2M12 7.9h.01" stroke-width="1.9"/>'),
  copy: svg('<rect x="8.5" y="8.5" width="11" height="11" rx="2"/><path d="M15.5 8.5V6.5a2 2 0 0 0-2-2h-7a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h2"/>'),
  studio: svg('<path d="M12 3.5c.6 4.2 2.3 5.9 6.5 6.5-4.2.6-5.9 2.3-6.5 6.5-.6-4.2-2.3-5.9-6.5-6.5 4.2-.6 5.9-2.3 6.5-6.5z"/><path d="M18.5 15.5c.25 1.6.9 2.25 2.5 2.5-1.6.25-2.25.9-2.5 2.5-.25-1.6-.9-2.25-2.5-2.5 1.6-.25 2.25-.9 2.5-2.5z"/>'),
  caret: svg('<path d="m7.5 14 4.5-4.5 4.5 4.5"/>'),
} as const;

export type IconName = keyof typeof ICONS;
