import { recent } from "@/lib/events";

export const dynamic = "force-dynamic";

/** The latest webhook events, newest first; ?event=message filters by name. */
export async function GET(req: Request) {
  const want = new URL(req.url).searchParams.get("event");
  return Response.json(want ? recent().filter((e) => e.event === want) : recent());
}
