import { describe, expect, test } from "bun:test";
import { GitHub, githubDownloadAccept } from "./index";
import {
  MAXIMUM_GITHUB_DOWNLOAD_BYTES,
  MAXIMUM_GITHUB_JSON_BYTES,
} from "./provenance";

describe("GitHub binary download media types", () => {
  test("requests Actions artifact archives through the GitHub API media type", () => {
    expect(
      githubDownloadAccept(
        "https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/actions/artifacts/9249149008/zip",
      ),
    ).toBe("application/vnd.github+json");
  });

  test("requests release assets as octet streams", () => {
    expect(
      githubDownloadAccept(
        "https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/releases/assets/515824468",
      ),
    ).toBe("application/octet-stream");
  });

  test("bounds GitHub JSON by declared and actual bytes", async () => {
    const oversizedHeader = new GitHub(
      "token",
      (async () =>
        new Response("{}", {
          headers: {
            "content-length": String(MAXIMUM_GITHUB_JSON_BYTES + 1),
          },
        })) as typeof fetch,
    );
    await expect(
      oversizedHeader.get("/fixture", AbortSignal.timeout(1000)),
    ).rejects.toThrow("GitHub API response size is invalid");

    const oversizedBody = new GitHub(
      "token",
      (async () =>
        new Response("x".repeat(MAXIMUM_GITHUB_JSON_BYTES + 1), {
          headers: { "content-length": "2" },
        })) as typeof fetch,
    );
    await expect(
      oversizedBody.get("/fixture", AbortSignal.timeout(1000)),
    ).rejects.toThrow("GitHub API response size is invalid");
  });

  test("bounds GitHub downloads by the caller's verified size", async () => {
    const github = new GitHub(
      "token",
      (async () =>
        new Response("12345", {
          headers: { "content-length": "1" },
        })) as typeof fetch,
    );
    await expect(
      github.bytes(
        "https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/releases/assets/1",
        AbortSignal.timeout(1000),
        4,
      ),
    ).rejects.toThrow("GitHub download size is invalid");
  });

  test("rejects an unsafe GitHub download limit before fetch", async () => {
    let fetches = 0;
    const github = new GitHub(
      "token",
      (async () => {
        fetches++;
        return new Response("x");
      }) as typeof fetch,
    );
    await expect(
      github.bytes(
        "https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/releases/assets/1",
        AbortSignal.timeout(1000),
        MAXIMUM_GITHUB_DOWNLOAD_BYTES + 1,
      ),
    ).rejects.toThrow("GitHub download size limit is invalid");
    expect(fetches).toBe(0);
  });
});
