import { describe, expect, test } from "bun:test";
import {
  MAXIMUM_REGISTRY_MANIFEST_BYTES,
  RegistryHTTPError,
} from "./provenance";
import {
  MAXIMUM_REGISTRY_TOKEN_BYTES,
  PublicGhcrRegistry,
  rawDigest,
} from "./registry";

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
    try {
      await new PublicGhcrRegistry(fetcher).manifest(
        "latest",
        AbortSignal.timeout(1000),
      );
      throw new Error("expected registry failure");
    } catch (error) {
      expect(error).toBeInstanceOf(RegistryHTTPError);
      expect((error as RegistryHTTPError).status).toBe(500);
    }
  });
  test("bounds manifests by declared and actual bytes", async () => {
    const declared = new PublicGhcrRegistry(
      (async () =>
        new Response("{}", {
          headers: {
            "content-length": String(MAXIMUM_REGISTRY_MANIFEST_BYTES + 1),
          },
        })) as typeof fetch,
    );
    await expect(
      declared.manifest("latest", AbortSignal.timeout(1000)),
    ).rejects.toThrow("registry manifest size is invalid");

    const actual = new PublicGhcrRegistry(
      (async () =>
        new Response("x".repeat(MAXIMUM_REGISTRY_MANIFEST_BYTES + 1), {
          headers: { "content-length": "2" },
        })) as typeof fetch,
    );
    await expect(
      actual.manifest("latest", AbortSignal.timeout(1000)),
    ).rejects.toThrow("registry manifest size is invalid");
  });
  test("bounds registry token responses before parsing", async () => {
    const fetcher = (async (url: string) => {
      if (url.includes("/token?"))
        return new Response("{}", {
          headers: {
            "content-length": String(MAXIMUM_REGISTRY_TOKEN_BYTES + 1),
          },
        });
      return new Response("", { status: 401 });
    }) as typeof fetch;
    await expect(
      new PublicGhcrRegistry(fetcher).manifest(
        "latest",
        AbortSignal.timeout(1000),
      ),
    ).rejects.toThrow("registry token response size is invalid");
  });
  test("computes digest over exact raw body", () =>
    expect(rawDigest(new TextEncoder().encode("x"))).not.toBe(
      rawDigest(new TextEncoder().encode("x\n")),
    ));
});
