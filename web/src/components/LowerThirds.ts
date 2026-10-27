// Polished name labels on call tiles [SL-P5-2]: when the host turns on
// "Name and title labels", each tile shows a lower third (accent strip,
// name, title) in place of the plain name pill. Text comes from brandStore
// and is set with textContent only. Presentation only: tiles keep their
// ids, owners and the .tile-name element other code and tests read.
import { brandStore } from '../lib/branding';

const SUFFIX = /\s*(\/\s*(Host|Participant|Producer · not recorded)|\(Producer, not recorded\))$/;

function displayName(tile: HTMLElement): string {
  return (tile.querySelector('.tile-name')?.textContent || '').replace(SUFFIX, '').trim();
}

function participantOf(tile: HTMLElement): string {
  if (tile.dataset.participantId) return tile.dataset.participantId;
  return tile.id === 'local-tile' ? brandStore.selfId : '';
}

/** Keeps tile lower thirds in sync with the brand and the grid. */
export function mountTileLowerThirds(grid: HTMLElement | null): void {
  if (!grid) return;
  let queued = false;
  const render = () => {
    queued = false;
    const brand = brandStore.get();
    const on = brand.tileLabels;
    grid.classList.toggle('branded-labels', on);
    if (grid.style.getPropertyValue('--brand-accent') !== brand.accent) grid.style.setProperty('--brand-accent', brand.accent);
    for (const tile of grid.querySelectorAll<HTMLElement>('.tile')) {
      let lt = tile.querySelector<HTMLElement>(':scope > .tile-lower-third');
      // A screen share keeps its plain label.
      const pid = participantOf(tile);
      if (!on || !pid || tile.classList.contains('tile-screen')) { lt?.remove(); tile.classList.remove('has-lt-title'); continue; }
      const text = brandStore.text(pid, displayName(tile));
      if (!lt) {
        lt = document.createElement('div');
        lt.className = 'tile-lower-third';
        lt.setAttribute('aria-hidden', 'true'); // .tile-name stays the accessible label
        const name = document.createElement('span');
        name.className = 'lt-name';
        const title = document.createElement('span');
        title.className = 'lt-title';
        lt.append(name, title);
        tile.append(lt);
      }
      const nameEl = lt.querySelector<HTMLElement>('.lt-name')!, titleEl = lt.querySelector<HTMLElement>('.lt-title')!;
      if (nameEl.textContent !== text.name) nameEl.textContent = text.name;
      if (titleEl.textContent !== text.title) titleEl.textContent = text.title;
      titleEl.hidden = !text.title;
      tile.classList.toggle('has-lt-title', !!text.title); // lifts the signal bars (branding.css)
    }
  };
  const schedule = () => { if (!queued) { queued = true; requestAnimationFrame(render); } };
  brandStore.subscribe(schedule);
  new MutationObserver(records => {
    // Ignore our own label writes; react to tiles and names changing.
    if (records.every(r => (r.target as HTMLElement).closest?.('.tile-lower-third') || [...r.addedNodes, ...r.removedNodes].every(n => (n as HTMLElement).classList?.contains('tile-lower-third')))) return;
    schedule();
  }).observe(grid, { childList: true, subtree: true, characterData: true });
  schedule();
}
