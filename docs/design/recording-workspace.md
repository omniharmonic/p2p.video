# p2p.video recording workspace

The interface is a restrained recording instrument: graphite surfaces, precise edges, a broad source image, and an icy accent. Red marks actual recording actions and failures. Labels describe observed state; decorative telemetry is excluded.

## Visual system

Superseded in detail on 2026-10-25 by the consolidated design system
([design-system.md](design-system.md), tokens in `web/src/tokens.css`) and the
Meet-style call layout ([call-screen.md](call-screen.md)) [SL-UX-1]. The
character is unchanged: graphite surfaces, an ice accent, red only for
recording and ending, Helvetica Neue with tabular numerals. What changed:
one type scale with an 11px minimum (was 9–10px in places), radii that grow
with the surface (8px controls, 12px cards and tiles, 16px dialogs) instead of
3px everywhere, and round 44px call controls.

## Workspaces

Source setup pairs a large camera preview, with the camera and microphone pickers and level meter directly under it, with a details card (name, uncompressed-mic option, Join, then the pre-flight summary). On the host home the same setup sits beside Rooms and Recent meetings. Mobile places the preview above the inspector.

The live studio puts source images above a quiet transport bar (see call-screen.md). Recording controls are visually distinct from layout selection and delivery actions. Layout switching only changes the preview; it does not imply a live broadcast or alter the local originals.

Transfers are a source ledger. Local bytes, host bytes, receipts and media preparation remain distinct. Export is enabled only after fresh host status confirms all expected sources. The delivery overlay lists generated files and keeps the synchronization review requirement visible.

Settings expose working capture targets and export options. Unsupported engines and fine acoustic alignment are labeled unavailable. Fixed audio conversion is stated as delivery formatting, not restored capture fidelity.

## Interaction requirements

Preserve existing DOM IDs and recording state contracts. Device choices lock on joining. Host-only actions follow the admitted server role. Input errors, incomplete transfers, and storage failures remain visible. Do not replace real connection or receipt state with decorative indicators.

Keyboard focus is explicit; dialogs have names and close actions. Narrow layouts stack controls and avoid horizontal page overflow. Reduced motion is respected.

