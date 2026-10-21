import { randomUUID } from "node:crypto";
import { instance, wax } from "@/lib/wax";

/** Queues a text message; wax answers 202 with the outbox id. */
export async function POST(req: Request) {
  const { to, text } = await req.json();
  try {
    const res = await wax("POST", `/v1/instances/${instance}/messages`, {
      to,
      text,
      client_id: randomUUID(), // makes a retried request safe
    });
    return Response.json(await res.json(), { status: 202 });
  } catch (e) {
    return Response.json({ error: String((e as Error).message) }, { status: 502 });
  }
}
