import { describe, expect, test } from "bun:test";
import { PublicGhcrRegistry, rawDigest } from "./registry";

describe("public GHCR client", () => {
  test("performs bearer challenge and strips content-type parameters", async () => {
    const calls: any[] = [];
    const body = new TextEncoder().encode("{}");
    const fetcher: any = async (url: string, init: any) => {
      calls.push([url, init]);
      if (url.includes("/token?"))
        return new Response(JSON.stringify({ token: "private-token" }));
      if (calls.length === 1) return new Response("", { status: 401 });
      return new Response(body, {
        headers: {
          "content-type":
            "application/vnd.oci.image.index.v1+json; charset=utf-8",
          "docker-content-digest": rawDigest(body),
        },
      });
    };
    const got = await new PublicGhcrRegistry(fetcher).manifest(
      "v1",
      AbortSignal.timeout(1000),
    );
    expect(got.mediaType).toBe("application/vnd.oci.image.index.v1+json");
    expect(calls[1][0]).toContain(
      "scope=repository:unstableneutron/cli-proxy-api-plus:pull",
    );
    expect(calls[2][1].headers.Authorization).toBe("Bearer private-token");
  });
  test("does not request token when public manifest succeeds", async () => {
    let count = 0;
    const fetcher: any = async () => {
      count++;
      return new Response("{}", {
        headers: { "content-type": "application/vnd.oci.image.index.v1+json" },
      });
    };
    await new PublicGhcrRegistry(fetcher).manifest(
      "latest",
      AbortSignal.timeout(1000),
    );
    expect(count).toBe(1);
  });
  test("propagates registry errors as network/API errors", async () => {
    const fetcher: any = async () => new Response("", { status: 500 });
    await expect(
      new PublicGhcrRegistry(fetcher).manifest(
        "latest",
        AbortSignal.timeout(1000),
      ),
    ).rejects.toThrow("registry returned 500");
  });
  test("computes digest over exact raw body", () =>
    expect(rawDigest(new TextEncoder().encode("x"))).not.toBe(
      rawDigest(new TextEncoder().encode("x\n")),
    ));
});
