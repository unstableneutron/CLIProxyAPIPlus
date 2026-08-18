import { describe, expect, test } from "bun:test";
import { readBoundedResponse } from "./bounded-response";

const rejected = (message: string) => new Error(message);

describe("bounded response reader", () => {
  test("accepts a body at the exact limit", async () => {
    const response = new Response("1234", {
      headers: { "content-length": "4" },
    });
    expect(
      new TextDecoder().decode(
        await readBoundedResponse(response, 4, "fixture", rejected),
      ),
    ).toBe("1234");
  });

  test("rejects an oversized declared length", async () => {
    const response = new Response(
      new ReadableStream({
        pull(controller) {
          controller.enqueue(new Uint8Array([1]));
          controller.close();
        },
      }),
      { headers: { "content-length": "5" } },
    );
    await expect(
      readBoundedResponse(response, 4, "fixture", rejected),
    ).rejects.toThrow("fixture size is invalid");
  });

  test("rejects a dishonest short length when actual bytes exceed the limit", async () => {
    const response = new Response("12345", {
      headers: { "content-length": "1" },
    });
    await expect(
      readBoundedResponse(response, 4, "fixture", rejected),
    ).rejects.toThrow("fixture size is invalid");
  });

  test("rejects malformed declared lengths", async () => {
    const response = new Response("x", {
      headers: { "content-length": "unknown" },
    });
    await expect(
      readBoundedResponse(response, 4, "fixture", rejected),
    ).rejects.toThrow("fixture size is invalid");
  });
});
