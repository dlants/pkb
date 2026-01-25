export function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export async function pollUntil<T>(
  fn: (() => Promise<T>) | (() => T),
  opts: { timeout: number; message?: string } = { timeout: 1000 },
): Promise<T> {
  const start = new Date().getTime();
  let lastError: Error | undefined;
  while (true) {
    if (new Date().getTime() - start > opts.timeout) {
      if (opts.message) {
        throw new Error(opts.message);
      }

      if (lastError) {
        throw lastError;
      }

      throw new Error(`pollUntil timeout`);
    }

    try {
      const res = fn();
      const val = res && (res as Promise<unknown>).then ? await res : res;
      return val;
    } catch (e) {
      lastError = e as Error;
    }

    await delay(100);
  }
}

export class Defer<T> {
  public promise: Promise<T>;
  public resolve!: (value: T) => void;
  public reject!: (reason: unknown) => void;
  public resolved = false;

  constructor() {
    this.promise = new Promise((resolve, reject) => {
      this.resolve = (value: T) => {
        this.resolved = true;
        resolve(value);
      };
      this.reject = (reason: unknown) => {
        this.resolved = true;
        reject(reason);
      };
    });
  }
}
