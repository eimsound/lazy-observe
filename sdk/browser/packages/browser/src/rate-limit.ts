export class MinuteRateLimiter {
  private windowStarted = 0;
  private count = 0;
  constructor(private readonly limit: number, private readonly now: () => number = () => Date.now()) {}
  take(): boolean {
    const current = this.now();
    if (current - this.windowStarted >= 60_000) { this.windowStarted = current; this.count = 0; }
    if (this.count >= this.limit) return false;
    this.count += 1;
    return true;
  }
}
