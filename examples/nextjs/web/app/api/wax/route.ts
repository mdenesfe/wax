// The webhook receiver: wax posts every event of the account here.

import { createHmac, timingSafeEqual } from "node:crypto";
import { add } from "@/lib/events";

export async function POST(req: Request) {
  // The signature covers the raw body, so read it before parsing.
  const body = Buffer.from(await req.arrayBuffer());
  const got = Buffer.from(req.headers.get("x-wax-signature")?.replace(/^sha256=/, "") ?? "", "hex");
  const want = createHmac("sha256", process.env.WAX_WEBHOOK_SECRET!).update(body).digest();
  if (got.length !== want.length || !timingSafeEqual(got, want)) {
    return new Response("bad signature", { status: 401 });
  }
  const { event, timestamp, data } = JSON.parse(body.toString("utf8"));
  // Answer quickly: deliveries of one account are ordered, so a slow
  // receiver holds back the next event. Heavy work belongs in a queue.
  add({ delivery: req.headers.get("x-wax-delivery") ?? "", event, timestamp, data });
  return new Response(null, { status: 204 });
}
