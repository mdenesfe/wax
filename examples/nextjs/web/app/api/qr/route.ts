import { instance, wax, WaxError } from "@/lib/wax";

export const dynamic = "force-dynamic";

/** The current QR code as a PNG; 404 when none is pending. */
export async function GET() {
  try {
    const res = await wax("GET", `/v1/instances/${instance}/qr?format=png`);
    return new Response(res.body, { headers: { "Content-Type": "image/png", "Cache-Control": "no-store" } });
  } catch (e) {
    return new Response(null, { status: e instanceof WaxError && e.status === 404 ? 404 : 502 });
  }
}
