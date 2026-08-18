import { describe, expect, test } from "bun:test";
import { createHmac } from "node:crypto";
import {
  admitWebhook,
  MAXIMUM_WEBHOOK_BYTES,
  verifyGitHubSignature,
} from "./admission";

const secret = "s".repeat(32),
  id = "123e4567-e89b-42d3-a456-426614174000";
const encode = (value: string) => new TextEncoder().encode(value);
const headers = (body: Uint8Array, event = "release") => ({
  "x-github-event": event,
  "x-github-delivery": id,
  "x-hub-signature-256":
    "sha256=" + createHmac("sha256", secret).update(body).digest("hex"),
});

describe("webhook admission", () => {
  test("accepts exact raw-byte HMAC", () => {
    const b = encode('{"action":"published"}');
    expect(
      verifyGitHubSignature(b, headers(b)["x-hub-signature-256"], secret),
    ).toBeTrue();
  });
  test("rejects altered raw bytes", () => {
    const b = encode('{"action":"published"}');
    expect(
      verifyGitHubSignature(
        encode('{ "action":"published"}'),
        headers(b)["x-hub-signature-256"],
        secret,
      ),
    ).toBeFalse();
  });
  test("rejects missing signature", () =>
    expect(verifyGitHubSignature(encode("{}"), undefined, secret)).toBeFalse());
  test("rejects malformed signature", () =>
    expect(
      verifyGitHubSignature(encode("{}"), "sha256=no", secret),
    ).toBeFalse());
  test("rejects short configured secret", () =>
    expect(
      verifyGitHubSignature(encode("{}"), "sha256=" + "0".repeat(64), "short"),
    ).toBeFalse());
  test("rejects an oversized release payload before HMAC", () => {
    const b = new Uint8Array(MAXIMUM_WEBHOOK_BYTES + 1);
    expect(
      verifyGitHubSignature(b, "sha256=" + "0".repeat(64), secret),
    ).toBeFalse();
    expect(() =>
      admitWebhook(
        {
          "x-github-event": "release",
          "x-github-delivery": id,
          "x-hub-signature-256": "sha256=" + "0".repeat(64),
        },
        b,
        secret,
      ),
    ).toThrow("request body too large");
  });
  test("ignores non-release before HMAC", () =>
    expect(
      admitWebhook({ "x-github-event": "push" }, encode("bad"), "short"),
    ).toEqual({ kind: "ignored" }));
  test("accepts a standard UUID delivery", () => {
    const b = encode('{"action":"published"}');
    expect(admitWebhook(headers(b), b, secret).kind).toBe("release");
  });
  test("accepts GitHub's stable released lifecycle action", () => {
    const b = encode('{"action":"released"}');
    expect(admitWebhook(headers(b), b, secret).kind).toBe("release");
  });
  test("rejects arbitrary delivery IDs", () => {
    const b = encode('{"action":"published"}');
    expect(() =>
      admitWebhook(
        { ...headers(b), "x-github-delivery": "delivery" },
        b,
        secret,
      ),
    ).toThrow("delivery ID invalid");
  });
  test("rejects invalid HMAC", () => {
    const b = encode('{"action":"published"}');
    expect(() =>
      admitWebhook(
        { ...headers(b), "x-hub-signature-256": "sha256=" + "0".repeat(64) },
        b,
        secret,
      ),
    ).toThrow("signature invalid");
  });
  test("rejects invalid JSON", () => {
    const b = encode("{");
    expect(() => admitWebhook(headers(b), b, secret)).toThrow(
      "request body invalid",
    );
  });
  test("rejects invalid UTF-8", () => {
    const b = new Uint8Array([0xff]);
    expect(() => admitWebhook(headers(b), b, secret)).toThrow(
      "request body invalid",
    );
  });
  test("rejects JSON arrays", () => {
    const b = encode("[]");
    expect(() => admitWebhook(headers(b), b, secret)).toThrow(
      "request body invalid",
    );
  });
  test.each(["created", "edited", "prereleased", "unpublished", "deleted"])(
    "rejects non-publication action %s",
    (action) => {
      const b = encode(JSON.stringify({ action }));
      expect(() => admitWebhook(headers(b), b, secret)).toThrow(
        "action is not a stable release publication",
      );
    },
  );
  test("rejects a spoofed stable action with a mismatched HMAC", () => {
    const published = encode('{"action":"published"}');
    const released = encode('{"action":"released"}');
    expect(() => admitWebhook(headers(published), released, secret)).toThrow(
      "signature invalid",
    );
  });
});
