import { describe, expect, test } from "bun:test";
import { parseUpstreamState } from "./provenance";

const sha = "a".repeat(40),
  fp = "14e308bd7b8de821e7d600e938543cbec944f6fb",
  original = "v7.2.132",
  plus = "v7.2.127-3";
const syncID = `original-${original}_plus-${plus}`;
const values: Record<string, string> = {
  SCHEMA_VERSION: "2",
  SYNC_ID: syncID,
  PLAN_FINGERPRINT: fp,
  BASE_FORK_COMMIT: sha,
  ORIGINAL_REPOSITORY: "router-for-me/CLIProxyAPI",
  ORIGINAL_TAG: original,
  ORIGINAL_COMMIT: sha,
  PLUS_REPOSITORY: "kaitranntt/CLIProxyAPIPlus",
  PLUS_TAG: plus,
  PLUS_TAG_COMMIT: sha,
  PLUS_HEAD_COMMIT: sha,
  PLUS_HEAD_INCLUDED: "false",
  MODELS_REPOSITORY: "router-for-me/models",
  MODELS_COMMIT: sha,
  EXPECTED_FORK_TAG: "v7.2.132-unstableneutron.0",
  CANDIDATE_BRANCH: `upstream-sync/${syncID}-${fp.slice(0, 12)}`,
};
const bytes = (change: Record<string, string> = {}, extra = "") =>
  new TextEncoder().encode(
    Object.entries({ ...values, ...change })
      .map(([k, v]) => `${k}=${v}`)
      .join("\n") +
      "\n" +
      extra,
  );

describe("upstream state fixture", () => {
  test("accepts exact real schema", () =>
    expect(parseUpstreamState(bytes()).SCHEMA_VERSION).toBe("2"));
  test("rejects wrong schema", () =>
    expect(() => parseUpstreamState(bytes({ SCHEMA_VERSION: "1" }))).toThrow());
  test("rejects extra keys", () =>
    expect(() => parseUpstreamState(bytes({}, "EXTRA=x\n"))).toThrow());
  test("rejects missing keys", () => {
    const x = { ...values };
    delete x.BASE_FORK_COMMIT;
    expect(() =>
      parseUpstreamState(
        new TextEncoder().encode(
          Object.entries(x)
            .map(([k, v]) => `${k}=${v}`)
            .join("\n"),
        ),
      ),
    ).toThrow();
  });
  test("rejects duplicate keys", () =>
    expect(() => parseUpstreamState(bytes({}, "SYNC_ID=x\n"))).toThrow());
  test("rejects wrong original repository", () =>
    expect(() =>
      parseUpstreamState(bytes({ ORIGINAL_REPOSITORY: "evil/repo" })),
    ).toThrow());
  test("rejects wrong plus repository", () =>
    expect(() =>
      parseUpstreamState(bytes({ PLUS_REPOSITORY: "evil/repo" })),
    ).toThrow());
  test("rejects wrong models repository", () =>
    expect(() =>
      parseUpstreamState(
        bytes({ MODELS_REPOSITORY: "kaitranntt/CLIProxyAPIPlus" }),
      ),
    ).toThrow());
  test("rejects malformed SHA", () =>
    expect(() =>
      parseUpstreamState(bytes({ BASE_FORK_COMMIT: "abc" })),
    ).toThrow());
  test("rejects malformed boolean", () =>
    expect(() =>
      parseUpstreamState(bytes({ PLUS_HEAD_INCLUDED: "yes" })),
    ).toThrow());
  test("rejects sync ID mismatch", () =>
    expect(() => parseUpstreamState(bytes({ SYNC_ID: "other" }))).toThrow());
  test("rejects branch fingerprint mismatch", () =>
    expect(() =>
      parseUpstreamState(bytes({ CANDIDATE_BRANCH: "upstream-sync/nope" })),
    ).toThrow());
  test("rejects invalid UTF-8", () =>
    expect(() => parseUpstreamState(new Uint8Array([0xff]))).toThrow(
      "state encoding",
    ));
  test("bounds state bytes", () =>
    expect(() => parseUpstreamState(new Uint8Array(64001))).toThrow(
      "state size",
    ));
});
