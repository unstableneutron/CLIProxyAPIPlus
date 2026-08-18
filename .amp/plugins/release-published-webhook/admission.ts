import { createHmac, timingSafeEqual } from "node:crypto";
import { RejectedDelivery } from "./provenance";

const DELIVERY_ID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
export const MAXIMUM_WEBHOOK_BYTES = 1_000_000;

export type Admission =
  | { kind: "ignored" }
  | { kind: "release"; deliveryID: string; payload: Record<string, unknown> };

export function verifyGitHubSignature(
  body: Uint8Array,
  supplied: string | undefined,
  secret: string,
): boolean {
  if (
    body.byteLength > MAXIMUM_WEBHOOK_BYTES ||
    secret.length < 32 ||
    !/^sha256=[0-9a-f]{64}$/.test(supplied ?? "")
  )
    return false;
  const actual = Buffer.from(supplied!.slice(7), "hex");
  const expected = createHmac("sha256", secret).update(body).digest();
  return actual.length === expected.length && timingSafeEqual(actual, expected);
}

export function admitWebhook(
  headers: Record<string, string | undefined>,
  body: Uint8Array,
  secret: string,
): Admission {
  if (headers["x-github-event"] !== "release") return { kind: "ignored" };
  if (body.byteLength > MAXIMUM_WEBHOOK_BYTES)
    throw new RejectedDelivery("request body too large");
  const deliveryID = headers["x-github-delivery"];
  if (!deliveryID || !DELIVERY_ID.test(deliveryID))
    throw new RejectedDelivery("delivery ID invalid");
  if (!verifyGitHubSignature(body, headers["x-hub-signature-256"], secret)) {
    throw new RejectedDelivery("signature invalid");
  }
  let value: unknown;
  try {
    value = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(body));
  } catch {
    throw new RejectedDelivery("request body invalid");
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new RejectedDelivery("request body invalid");
  }
  const payload = value as Record<string, unknown>;
  if (payload.action !== "published" && payload.action !== "released")
    throw new RejectedDelivery("action is not a stable release publication");
  return { kind: "release", deliveryID, payload };
}
