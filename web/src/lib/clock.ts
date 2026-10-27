import { ClockPingPayload, ClockPongPayload } from './protocol';

export interface ClockFit {
  a: number; // intercept (offset in microseconds)
  b: number; // slope (drift multiplier)
  rttP10Ms: number;
}

/**
 * A round trip shorter than the page's timer resolution reads as 0 (Safari,
 * WKWebView and Firefox coarsen performance.now() to 1 ms or more, so a
 * loopback ping often measures 0 µs). It is still a measurement: report at
 * least 1 µs, because the host reads rttP10Ms = 0 as "no timing samples" and
 * refuses to align the track [SL-P1-12, SL-P2-3].
 */
export const MIN_MEASURED_RTT_US = 1;

export class ClockSync {
  private samples: Array<{ localT0: number; hostEst: number; rttUs: number }> = [];
  private fit: ClockFit = { a: 0, b: 1.0, rttP10Ms: 0 };

  public createPing(): ClockPingPayload {
    return {
      t0: Math.round(performance.now() * 1000),
    };
  }

  public handlePong(pong: ClockPongPayload): void {
    const t3 = Math.round(performance.now() * 1000);
    const rttUs = Math.max(MIN_MEASURED_RTT_US, t3 - pong.t0);
    // Host time midpoint
    const hostEst = pong.t1 + Math.round((pong.t2 - pong.t1) / 2);
    const localMid = pong.t0 + Math.round(rttUs / 2);

    this.samples.push({ localT0: localMid, hostEst, rttUs });
    if (this.samples.length > 50) {
      this.samples.shift();
    }

    this.recomputeFit();
  }

  public toHostUs(localPerfTimeMs: number): number {
    const localUs = localPerfTimeMs * 1000;
    return Math.round(this.fit.a + this.fit.b * localUs);
  }

  public getFit(): ClockFit {
    return { ...this.fit };
  }

  private recomputeFit(): void {
    if (this.samples.length < 2) {
      if (this.samples.length === 1) {
        this.fit.a = this.samples[0].hostEst - this.samples[0].localT0;
        this.fit.b = 1.0;
        this.fit.rttP10Ms = this.samples[0].rttUs / 1000;
      }
      return;
    }

    // Sort by RTT and pick the lowest-RTT 50% samples
    const sorted = [...this.samples].sort((x, y) => x.rttUs - y.rttUs);
    const best = sorted.slice(0, Math.max(2, Math.floor(sorted.length * 0.5)));

    let sumX = 0;
    let sumY = 0;
    let sumXY = 0;
    let sumXX = 0;
    const n = best.length;

    for (const s of best) {
      sumX += s.localT0;
      sumY += s.hostEst;
      sumXY += s.localT0 * s.hostEst;
      sumXX += s.localT0 * s.localT0;
    }

    const denom = n * sumXX - sumX * sumX;
    if (Math.abs(denom) > 1e-6) {
      const b = (n * sumXY - sumX * sumY) / denom;
      const a = (sumY - b * sumX) / n;
      this.fit.a = a;
      this.fit.b = b;
    } else {
      this.fit.a = (sumY / n) - (sumX / n);
      this.fit.b = 1.0;
    }
    this.fit.rttP10Ms = best[0].rttUs / 1000;
  }
}
