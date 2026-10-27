# p2p.video design system

Set 2026-10-25 [SL-UX-1]. One token set and one component kit for the whole web
client. Source of truth: `web/src/tokens.css`. Every other stylesheet uses its
variables. Feature files position their own elements; they don't define colours,
radii, shadows or z-index numbers of their own.

The product is a call first and a recording studio second. The look stays the
graphite instrument from `recording-workspace.md`, now applied consistently:
graphite surfaces, an ice accent for "on" and "selected", red only for
recording and for ending the meeting.

## Stylesheet order

`main.ts` imports CSS in this order, so the cascade is predictable:

1. `tokens.css`: tokens, base elements, shared components
2. `studio.css`: page structure (header, setup screens, stage, tiles, dialogs, drawers)
3. `preflight.css`
4. component CSS imported by components (`livestream.css`, `netquality.css`, `ecosystem.css`, …). The
   app modules in `web/src/app/` use the components, so `main.ts` also imports each CSS-importing
   component, in this historical order, before the app modules [SL-TECH-1]
5. `call-features.css`, `views.css`, `home.css`, `captions.css`
6. `call.css`: the in-call shell, dock and More menu. Last, because it arranges controls the other files define.

## Tokens

### Colour

| Token | Value | Use |
|---|---|---|
| `--sl-canvas` | `#0b0f13` | page background |
| `--sl-surface-1` | `#12181e` | cards, panels, drawers, popovers, dialogs |
| `--sl-surface-2` | `#19212a` | raised controls (dock buttons, secondary buttons) |
| `--sl-surface-3` | `#232d37` | hover |
| `--sl-inset` | `#0e1419` | inputs, wells |
| `--sl-stage` | `#070a0d` | behind video |
| `--sl-scrim` | `rgba(3,7,11,.72)` | modal and drawer backdrops |
| `--sl-line` / `--sl-line-strong` | `#26313a` / `#3b4853` | dividers / control borders |
| `--sl-text` / `-2` / `-3` | `#e8eef1` / `#a9b6c0` / `#8794a0` | primary / secondary / tertiary text |
| `--sl-accent` / `-strong` / `-soft` / `-line` | `#bfdbe8` … | "on", selected, primary action, focus |
| `--sl-on-accent` | `#0d1a21` | text on accent |
| `--sl-rec` | `#e5524e` | the recording dot |
| `--sl-rec-fill` / `-soft` / `-line` / `-text` | `#b8403c` … | End meeting, Record anyway, recording banner |
| `--sl-off-bg` / `-line` / `-text` | `#4a2427` … | mic or camera off |
| `--sl-warn`, `--sl-ok`, `--sl-danger` | `#e0c08f`, `#9cc4b1`, `#ef8b88` | status (hand raised, ready, errors) |

Contrast, measured: text 16.4:1, text-2 9.3:1, text-3 6.2:1 on canvas (5.2:1 on
surface-2); on-accent on accent 12.2:1; white on rec-fill 5.5:1; toast text
15.3:1. Tertiary text is never smaller than 11px.

There is one theme (dark). A video call is watched in dim rooms and the video
is the brightest thing on screen; a light theme isn't planned.

### Type

Helvetica Neue with system fallbacks (`--sl-font`), `--sl-mono` for links and
codes. One scale, 11px minimum:

`--fs-xs` 11 · `--fs-sm` 12 · `--fs-md` 13 · `--fs-base` 14 · `--fs-lg` 16 ·
`--fs-xl` 20 · `--fs-2xl` 28 · `--fs-3xl` 36. Line heights: `--lh-tight` 1.2
(headings), `--lh-ui` 1.4 (controls), `--lh-body` 1.55 (paragraphs). Weights 400,
500 for headings and controls, 600 for emphasis. Timers use tabular numerals.

### Space, radii, elevation, layers, motion

- Space: 4px grid, `--sp-1` 4 to `--sp-8` 48.
- Radii grow with the surface: `--r-1` 4 (chips, kbd), `--r-2` 8 (buttons,
  inputs), `--r-3` 12 (cards, tiles, popovers), `--r-4` 16 (dialogs, sheets),
  `--r-pill` (dock controls, chips, tile labels).
- Elevation: `--shadow-1` (subtle), `--shadow-2` (popovers, admission card),
  `--shadow-3` (dialogs, drawers).
- Layers: tile overlays 1–6 inside the tile; `--z-dock` 20, `--z-popover` 60,
  `--z-sheet` 90 (mobile chat/people), `--z-drawer` 100 (settings,
  livestream), `--z-modal` 110 (export, transfers, setup guide),
  `--z-countdown` 120, `--z-toast` 130.
- Motion: `--dur-1` 120ms, `--dur-2` 180ms, `--ease`. Motion only answers an
  action (open, toggle, reaction) plus the recording-dot pulse. All of it is off
  under `prefers-reduced-motion`.

## Components

| Component | Class | Notes |
|---|---|---|
| Button | `.btn` + `-primary` / `-secondary` / `-quiet` / `-danger` / `-end` / `-destructive` / `-remove` | 40px, `-compact` 30px, `-block` full width |
| Icon button | `.icon-button` | 32px round, for close and overflow |
| Dock control | `.dock .collab-btn`, `.dock-ctl` | 44px circle (call.css), label kept as accessible name, tooltip from `data-tip` |
| Chip / badge | `.chip`, `.badge` (+`-success`, `-waiting`) | |
| Switch | `.switch[role=switch]` | ice when on |
| Card | `.glass-panel` | surface-1, `--r-3` |
| Popover | `.popover-surface` and the shared list in tokens.css | surface-1, line-strong, `--shadow-2` |
| Dialog | `.modal-card`, `.confirm-dialog`, `.room-dialog`, … | surface-1, `--r-4`, scrim with blur |
| Drawer | `.drawer-panel` | right side, `--r-4` on the leading corners |
| Toast | `.toast` | light pill, bottom centre above the dock |
| Notice | `.recording-banner`, `.captions-indicator`, `.admission-banner` | one row above the stage in a call |

## Icons

One set, `web/src/components/icons.ts`: 24-unit line icons, 1.7 stroke, round
caps and joins, inline SVG (no fonts, no CDNs), drawn at 20px. Controls whose
owners rewrite their text (mic, camera, Record, End, menu rows) get the same
icons as a CSS mask through `--icon`, set by `CallChrome.ts`, so the icon
survives text changes.

## Accessibility rules

- Every control has a visible `:focus-visible` ring (2px ice, 2px offset).
- Icon-only controls keep their text in the DOM as the accessible name
  (visually hidden or `font-size: 0`), with a hover/focus tooltip.
- Visual order in the dock matches DOM order, so Tab follows what you see.
- Text contrast ≥ 4.5:1 (see above). Minimum size 11px.
- `prefers-reduced-motion` disables transitions and animations.
