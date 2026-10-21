import { status } from "@/lib/wax";

export const dynamic = "force-dynamic";

/** The account's status: {"instance": null} until it is created. */
export async function GET() {
  try {
    return Response.json({ instance: await status() });
  } catch (e) {
    return Response.json({ error: String((e as Error).message) }, { status: 502 });
  }
}
