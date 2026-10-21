// Received webhook events, kept in memory for /api/events. A real application
// would store them (or queue the work) instead.

export type WaxEvent = {
  delivery: string;
  event: string;
  timestamp: string;
  data: Record<string, any>;
};

const g = globalThis as unknown as { waxEvents?: WaxEvent[] };
const events = (g.waxEvents ??= []);

/** add records an event once: retries carry the same delivery id. */
export function add(e: WaxEvent) {
  if (events.some((x) => x.delivery === e.delivery)) return;
  events.unshift(e);
  events.length = Math.min(events.length, 200);
}

/** recent returns the newest events first. */
export function recent(): WaxEvent[] {
  return events;
}
