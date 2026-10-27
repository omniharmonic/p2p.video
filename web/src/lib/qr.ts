// QR codes for phone camera links [SL-P1-9], drawn on this device with the
// vendored MIT encoder (src/vendor/qrcodegen.ts). No network, no CDN.
import { qrcodegen } from '../vendor/qrcodegen';

export interface QrMatrix { size: number; modules: boolean[][] }

/** Encodes text (UTF-8 bytes) with medium error correction. */
export function qrMatrix(text: string): QrMatrix {
  const qr = qrcodegen.QrCode.encodeText(text, qrcodegen.QrCode.Ecc.MEDIUM);
  const modules: boolean[][] = [];
  for (let y = 0; y < qr.size; y++) {
    const row: boolean[] = [];
    for (let x = 0; x < qr.size; x++) row.push(qr.getModule(x, y));
    modules.push(row);
  }
  return { size: qr.size, modules };
}

/**
 * SVG markup for a QR code: dark modules on a white field with the standard
 * 4-module quiet zone, one path, crisp at any size. `title` is escaped.
 */
export function qrSvg(text: string, title = 'QR code'): string {
  const { size, modules } = qrMatrix(text);
  const quiet = 4, dim = size + quiet * 2;
  let d = '';
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) if (modules[y][x]) d += `M${x + quiet},${y + quiet}h1v1h-1z`;
  }
  const esc = title.replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]!));
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${dim} ${dim}" shape-rendering="crispEdges" role="img" aria-label="${esc}"><title>${esc}</title><rect width="${dim}" height="${dim}" fill="#fff"/><path d="${d}" fill="#000"/></svg>`;
}
