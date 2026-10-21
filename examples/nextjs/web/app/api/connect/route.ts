import { connect } from "@/lib/wax";

/** Creates the account (or restarts pairing); the QR code follows. */
export async function POST() {
  try {
    await connect();
    return new Response(null, { status: 204 });
  } catch (e) {
    return Response.json({ error: String((e as Error).message) }, { status: 502 });
  }
}
