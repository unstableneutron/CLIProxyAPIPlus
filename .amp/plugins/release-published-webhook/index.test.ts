import { describe, expect, test } from "bun:test";
import { githubDownloadAccept } from "./index";

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
});
