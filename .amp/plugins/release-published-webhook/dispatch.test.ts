import { describe, expect, test } from "bun:test";
import { mkdtemp, readdir, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  dispatch,
  emptyState,
  notifyIssue,
  parseState,
  prompt,
  releaseKey,
  type State,
} from "./dispatch";
import {
  atomicClaim,
  HIGH_AGENT_MODE,
  initializeClaimsDirectory,
  runnerCreateOptions,
} from "./index";
const r: any = {
  repository: "unstableneutron/CLIProxyAPIPlus",
  repositoryID: 1247056725,
  releaseID: 3,
  releaseURL: "https://github.com/x",
  tag: "v1.2.3-unstableneutron.4",
  publishedAt: "2026-01-01T00:00:00Z",
  commit: "a".repeat(40),
  kind: "upstream",
  syncID: "s",
  planFingerprint: "b".repeat(40),
  imageDigest: "sha256:" + "c".repeat(64),
  architectures: {
    amd64: "sha256:" + "d".repeat(64),
    arm64: "sha256:" + "e".repeat(64),
  },
  workflowPath: ".github/workflows/upstream-sync-v2.yml",
  workflowRunID: 5,
  workflowRunAttempt: 1,
};
describe("dispatch primitives", () => {
  test("immutable key", () =>
    expect(releaseKey(r)).toContain(":3:v1.2.3-unstableneutron.4:"));
  test("state rejects malformed", () => {
    expect(() => parseState([])).toThrow();
    expect(parseState(undefined)).toEqual(emptyState());
  });
  test("rejects legacy raw errors so they cannot be re-saved", () => {
    const key = releaseKey(r);
    const malicious: any = {
      schemaVersion: 1,
      releases: {
        [key]: {
          key,
          ampEventID: "amp-legacy",
          githubDeliveryID: gh1,
          status: "creation-uncertain",
          error: "raw upstream error containing a secret",
          updatedAt: "2026-01-01T00:00:00.000Z",
        },
      },
      ampEvents: { "amp-legacy": key },
      githubDeliveries: { [gh1]: key },
    };
    let saves = 0;
    expect(() => parseState(malicious)).toThrow("state malformed");
    try {
      parseState(malicious);
      saves++;
    } catch {}
    expect(saves).toBe(0);
  });
  test("prompt excludes untrusted release text and is fail closed", () => {
    const p = prompt(r, "event", "T-source");
    for (const required of [
      `${r.tag}@${r.imageDigest}`, "openai/gpt-5.5", "gpt-5.6-luna",
      "gpt-5.6-sol", "cc/deepseek-v4-flash", "REST, SSE, and WebSocket",
      "127.0.0.1-only", "separate", "restart: no", "no Watchtower",
      "no production bind mounts or mutation", "baseline health", "rollback digest",
      "/healthz succeeds", "/health, /status, and /status/ return 404",
      "CPAMP loopback and public routed /health", "five meaningful minutes",
      "service only", "restore the compose backup", "failure diagnostics",
      "Never ship, patch, tag, release, or modify source", "do not repair tagged source",
    ]) expect(p).toContain(required);
    expect(p).not.toContain("SENTINEL");
    expect(p).not.toContain("orb");
    expect(p).not.toContain("parent");
  });
});

const tid = "T-01a00528-00c7-70fa-b34e-6d1833733852";
const gh1 = "123e4567-e89b-42d3-a456-426614174000";
const gh2 = "223e4567-e89b-42d3-a456-426614174000";
function harness() {
  let state: State = emptyState(),
    creates = 0,
    appends = 0,
    marker = false,
    claimed = false;
  const thread = {
    id: tid,
    append: async () => {
      appends++;
    },
    hasMarker: async () => marker,
  };
  return {
    store: {
      load: async () => structuredClone(state),
      save: async (s: State) => {
        state = structuredClone(s);
      },
      claim: async () => {
        if (claimed) return false;
        claimed = true;
        return true;
      },
    },
    spawner: {
      create: async () => {
        creates++;
        return thread;
      },
      get: () => thread,
    },
    counts: () => ({ creates, appends }),
    state: () => state,
    setMarker: () => {
      marker = true;
    },
  };
}
describe("durable dispatch", () => {
  test("real filesystem claim has one winner and durable private shape", async () => {
    const root = await mkdtemp(join(tmpdir(), "release-webhook-claim-"));
    try {
      const claims = await initializeClaimsDirectory(root);
      const key = releaseKey(r);
      const winners = await Promise.all(
        Array.from({ length: 8 }, () => atomicClaim(claims, key)),
      );
      expect(winners.filter(Boolean)).toHaveLength(1);
      const names = await readdir(claims);
      expect(names).toHaveLength(1);
      expect(names[0]).toMatch(/^[0-9a-f]{64}$/);
      expect((await stat(join(claims, names[0]))).mode & 0o777).toBe(0o600);
      expect(await atomicClaim(claims, key)).toBe(false);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });
  test("issue callback is sanitized and marker-deduplicated", async () => {
    const messages: string[] = [];
    const thread = {
      id: tid,
      append: async (message: string) => {
        messages.push(message);
      },
      hasMarker: async (marker: string) =>
        messages.some((message) => message.includes(marker)),
    };
    const key = releaseKey(r);
    expect(await notifyIssue(thread, "creation-uncertain", key)).toBe(true);
    expect(await notifyIssue(thread, "creation-uncertain", key)).toBe(false);
    expect(await notifyIssue(thread, "creation-uncertain", releaseKey({ ...r, releaseID: 4 }))).toBe(true);
    expect(messages).toHaveLength(2);
    expect(messages[0]).not.toContain(r.tag);
  });
  test("runner options are exact and create a fresh thread", () =>
    expect([HIGH_AGENT_MODE, runnerCreateOptions()]).toEqual([
      "high",
      { executor: { type: "runner", id: "vn3" } },
    ]));
  test("creates and appends exactly once", async () => {
    const h = harness();
    expect(
      (await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid)).outcome,
    ).toBe("dispatched");
    expect(h.counts()).toEqual({ creates: 1, appends: 1 });
  });
  test("atomic claim permits only one concurrent create", async () => {
    let state: State = emptyState(), claimed = false, arrivals = 0;
    let releaseBarrier!: () => void;
    const barrier = new Promise<void>((resolve) => { releaseBarrier = resolve; });
    const h = harness();
    const store = {
      load: async () => structuredClone(state),
      save: async (s: State) => { state = structuredClone(s); },
      claim: async () => {
        arrivals++;
        if (arrivals === 2) releaseBarrier();
        await barrier;
        if (claimed) return false;
        claimed = true;
        return true;
      },
    };
    const results = await Promise.all([
      dispatch(store, h.spawner, r, "amp-1", gh1, tid),
      dispatch(store, h.spawner, r, "amp-2", gh2, tid),
    ]);
    expect(results.map((x) => x.outcome).sort()).toEqual(["blocked", "dispatched"]);
    expect(h.counts().creates).toBe(1);
  });
  test("interrupted filesystem claim without state blocks creation", async () => {
    const h = harness();
    await h.store.claim(releaseKey(r));
    expect((await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid)).issue).toBe("existing-claim");
    expect(h.counts().creates).toBe(0);
  });
  test("same Amp and GitHub delivery is duplicate", async () => {
    const h = harness();
    await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid);
    expect(
      (await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid)).outcome,
    ).toBe("duplicate");
    expect(h.counts().creates).toBe(1);
  });
  test("same release with new deliveries never creates again", async () => {
    const h = harness();
    await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid);
    expect(
      (await dispatch(h.store, h.spawner, r, "amp-2", gh2, tid)).outcome,
    ).toBe("duplicate");
    expect(h.counts().creates).toBe(1);
  });
  test("Amp delivery rebind to another release blocks", async () => {
    const h = harness();
    await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid);
    expect(
      (
        await dispatch(
          h.store,
          h.spawner,
          { ...r, releaseID: 4 },
          "amp-1",
          gh2,
          tid,
        )
      ).outcome,
    ).toBe("blocked");
  });
  test("GitHub delivery rebind to another release blocks", async () => {
    const h = harness();
    await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid);
    expect(
      (
        await dispatch(
          h.store,
          h.spawner,
          { ...r, releaseID: 4 },
          "amp-2",
          gh1,
          tid,
        )
      ).outcome,
    ).toBe("blocked");
  });
  test("creation exception becomes uncertain and is never retried", async () => {
    const h = harness();
    let calls = 0;
    h.spawner.create = async () => {
      calls++;
      throw new Error("SECRET dependency failure");
    };
    expect(
      (await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid)).outcome,
    ).toBe("blocked");
    expect(
      (await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid)).outcome,
    ).toBe("blocked");
    expect(calls).toBe(1);
    expect(h.state().releases[releaseKey(r)].error).toBe("thread-create-failed");
    expect(JSON.stringify(h.state())).not.toContain("SECRET");
  });
  test("malicious append errors are sanitized in state", async () => {
    const h = harness();
    h.spawner.create = async () => ({
      id: tid,
      hasMarker: async () => false,
      append: async () => { throw new Error("STEAL-TOKEN payload=https://evil"); },
    });
    await expect(dispatch(h.store, h.spawner, r, "amp-1", gh1, tid)).rejects.toThrow("thread-append-failed");
    expect(h.state().releases[releaseKey(r)].error).toBe("thread-append-failed");
    expect(JSON.stringify(h.state())).not.toContain("STEAL-TOKEN");
  });
  test("marker recovery does not append a second prompt", async () => {
    const h = harness();
    await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid);
    const key = releaseKey(r);
    h.state().releases[key].status = "thread-created";
    h.setMarker();
    await dispatch(h.store, h.spawner, r, "amp-1", gh1, tid);
    expect(h.counts().appends).toBe(1);
  });
  test("deep parser rejects dangling maps and bad status", () => {
    expect(() =>
      parseState({
        schemaVersion: 1,
        releases: {},
        ampEvents: { x: "missing" },
        githubDeliveries: {},
      }),
    ).toThrow();
    const h = harness();
    const bad: any = h.state();
    bad.releases.x = {
      key: "x",
      ampEventID: "a",
      githubDeliveryID: "g",
      status: "oops",
      updatedAt: "now",
    };
    expect(() => parseState(bad)).toThrow();
  });
});
