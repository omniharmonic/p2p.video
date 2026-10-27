// Extra line icons for the home and Ready to join [SL-UX-2a]. Same drawing
// rules as icons.ts (24-unit, round caps, currentColor); kept here so the
// shared icon set stays untouched while the call UI is redesigned in parallel.
const svg = (d: string) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" focusable="false" aria-hidden="true">${d}</svg>`;

export const HOME_ICONS = {
  calendar: svg('<rect x="3.5" y="5" width="17" height="15" rx="2.5"/><path d="M3.5 9.5h17M8 3v4M16 3v4"/><path d="M8 13.5h.01M12 13.5h.01M16 13.5h.01M8 16.8h.01M12 16.8h.01" stroke-width="2.2"/>'),
  door: svg('<path d="M5.5 20.5V5a1.5 1.5 0 0 1 1.5-1.5h10A1.5 1.5 0 0 1 18.5 5v15.5"/><path d="M3.5 20.5h17"/><path d="M14.5 12.5h.01" stroke-width="2.4"/>'),
  recordings: svg('<rect x="3.5" y="5.5" width="17" height="13" rx="2.5"/><circle cx="12" cy="12" r="3" fill="currentColor" stroke="none"/>'),
  plus: svg('<path d="M12 5v14M5 12h14"/>'),
  link: svg('<path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/>'),
  keyboard: svg('<rect x="3" y="6.5" width="18" height="11" rx="1.5"/><path d="M7 10h.01M10.5 10h.01M14 10h.01M17.5 10h.01M8 14h8" stroke-width="1.9"/>'),
  video: svg('<rect x="3" y="6.5" width="12.5" height="11" rx="2"/><path d="m15.5 10.5 5-3v9l-5-3z"/>'),
  clock: svg('<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>'),
  solo: svg('<circle cx="12" cy="8.5" r="3.5"/><path d="M5.5 20a6.5 6.5 0 0 1 13 0"/><circle cx="18.5" cy="5.5" r="2" fill="currentColor" stroke="none"/>'),
  chevronDown: svg('<path d="m7 10 5 5 5-5"/>'),
  chevronLeft: svg('<path d="m14.5 6-6 6 6 6"/>'),
  chevronRight: svg('<path d="m9.5 6 6 6-6 6"/>'),
  back: svg('<path d="M19 12H5M11 6l-6 6 6 6"/>'),
  sparkle: svg('<path d="M12 3.5 13.8 9a2 2 0 0 0 1.2 1.2l5.5 1.8-5.5 1.8a2 2 0 0 0-1.2 1.2L12 20.5 10.2 15A2 2 0 0 0 9 13.8L3.5 12 9 10.2A2 2 0 0 0 10.2 9z"/>'),
  speaker: svg('<path d="M4 9.5h3.5L12 5.5v13l-4.5-4H4z"/><path d="M15.5 9a4 4 0 0 1 0 6M18 6.5a7.5 7.5 0 0 1 0 11"/>'),
  headphones: svg('<path d="M4 15v-3a8 8 0 0 1 16 0v3"/><rect x="3.5" y="14" width="4" height="6" rx="1.5"/><rect x="16.5" y="14" width="4" height="6" rx="1.5"/>'),
  check: svg('<path d="M5 12.5l4.5 4.5L19 7.5"/>'),
  live: svg('<circle cx="12" cy="12" r="2.2"/><path d="M7.8 7.8a6 6 0 0 0 0 8.4M16.2 7.8a6 6 0 0 1 0 8.4"/>'),
  history: svg('<path d="M4 12a8 8 0 1 0 2.4-5.7L4 8.5"/><path d="M4 4v4.5h4.5M12 8v4.5l3 1.8"/>'),
  download: svg('<path d="M12 4v11M7.5 10.5 12 15l4.5-4.5"/><path d="M4.5 16.5v2a1.5 1.5 0 0 0 1.5 1.5h12a1.5 1.5 0 0 0 1.5-1.5v-2"/>'),
} as const;

/** The empty-day illustration: an open calendar page with a small sun. Tokens only. */
export const EMPTY_DAY_ART = `<svg class="home-empty-art" viewBox="0 0 160 120" width="160" height="120" fill="none" aria-hidden="true" focusable="false">
  <circle cx="122" cy="30" r="14" fill="var(--sl-chip)"/>
  <path d="M122 8v6M122 46v6M100 30h6M138 30h6M106.5 14.5l4 4M133.5 41.5l4 4M137.5 14.5l-4 4M110.5 41.5l-4 4" stroke="var(--sl-accent-line)" stroke-width="2.4" stroke-linecap="round"/>
  <rect x="22" y="30" width="84" height="74" rx="12" fill="var(--sl-surface-1)" stroke="var(--sl-line-strong)" stroke-width="2"/>
  <path d="M22 50h84" stroke="var(--sl-line-strong)" stroke-width="2"/>
  <path d="M42 24v14M86 24v14" stroke="var(--sl-text-3)" stroke-width="3" stroke-linecap="round"/>
  <rect x="36" y="62" width="22" height="14" rx="4" fill="var(--sl-accent-soft)"/>
  <rect x="64" y="62" width="28" height="14" rx="4" fill="var(--sl-surface-2)"/>
  <rect x="36" y="82" width="38" height="10" rx="4" fill="var(--sl-surface-2)"/>
  <path d="M80 87c4 3 9 3 13 0" stroke="var(--sl-accent)" stroke-width="2.4" stroke-linecap="round"/>
</svg>`;
