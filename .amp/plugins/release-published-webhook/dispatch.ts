import type { VerifiedRelease } from "./provenance";
import { REPOSITORY_ID } from "./provenance";
import { createHash } from "node:crypto";
const THREAD_ID = /^T-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const GITHUB_DELIVERY_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const RELEASE_KEY = new RegExp(`^${REPOSITORY_ID}:[1-9][0-9]*:v[0-9]+\\.[0-9]+\\.[0-9]+-unstableneutron\\.[0-9]+:[0-9a-f]{40}:sha256:[0-9a-f]{64}$`);
const ISO_INSTANT = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;
export type Status =
  | "claimed"
  | "creation-uncertain"
  | "thread-created"
  | "append-failed"
  | "dispatched";
export interface Record {
  key: string;
  ampEventID: string;
  githubDeliveryID: string;
  status: Status;
  updatedAt: string;
  threadID?: string;
  error?: string;
}
export interface State {
  schemaVersion: 1;
  releases: globalThis.Record<string, Record>;
  ampEvents: globalThis.Record<string, string>;
  githubDeliveries: globalThis.Record<string, string>;
}
export interface Store {
  load(): Promise<State>;
  save(s: State): Promise<void>;
  /** Atomically and permanently claims an immutable release key. */
  claim(key: string): Promise<boolean>;
}
export interface Thread {
  id: string;
  append(s: string): Promise<void>;
  hasMarker(marker: string): Promise<boolean>;
}
export interface Spawner {
  create(): Promise<Thread>;
  get(id: string): Thread;
}
export const emptyState = (): State => ({
  schemaVersion: 1,
  releases: {},
  ampEvents: {},
  githubDeliveries: {},
});
export function parseState(v: unknown): State {
  if (v === undefined || v === null) return emptyState();
  if (!v || typeof v !== "object" || Array.isArray(v))
    throw new Error("release webhook state malformed");
  const s = v as State;
  if (
    s.schemaVersion !== 1 ||
    !plainMap(s.releases) ||
    !plainMap(s.ampEvents) ||
    !plainMap(s.githubDeliveries)
  )
    throw new Error("release webhook state schema unsupported");
  if (
    Object.keys(s).sort().join() !==
    ["ampEvents", "githubDeliveries", "releases", "schemaVersion"].sort().join()
  )
    throw new Error("release webhook state malformed");
  for (const [key, value] of Object.entries(s.releases)) {
    if (!value || typeof value !== "object" || Array.isArray(value))
      throw new Error("release webhook state malformed");
    const required = [
      "key",
      "ampEventID",
      "githubDeliveryID",
      "status",
      "updatedAt",
    ];
    const allowed = [...required, "threadID", "error"];
    const keys = Object.keys(value);
    if (
      keys.some((k) => !allowed.includes(k)) ||
      required.some((k) => !keys.includes(k)) ||
      !RELEASE_KEY.test(key) ||
      value.key !== key ||
      typeof value.ampEventID !== "string" ||
      !value.ampEventID ||
      typeof value.githubDeliveryID !== "string" ||
      !GITHUB_DELIVERY_ID.test(value.githubDeliveryID) ||
      typeof value.updatedAt !== "string" ||
      !ISO_INSTANT.test(value.updatedAt) ||
      new Date(value.updatedAt).toISOString() !== value.updatedAt ||
      ![
        "claimed",
        "creation-uncertain",
        "thread-created",
        "append-failed",
        "dispatched",
      ].includes(value.status)
    )
      throw new Error("release webhook state malformed");
    const needsThread = ["thread-created", "append-failed", "dispatched"].includes(value.status);
    if ((needsThread && !THREAD_ID.test(value.threadID)) || (!needsThread && value.threadID !== undefined))
      throw new Error("release webhook state malformed");
    const expectedError = value.status === "creation-uncertain"
      ? "thread-create-failed"
      : value.status === "append-failed"
        ? "thread-append-failed"
        : undefined;
    if (value.error !== expectedError)
      throw new Error("release webhook state malformed");
    if (
      s.ampEvents[value.ampEventID] !== key ||
      s.githubDeliveries[value.githubDeliveryID] !== key
    )
      throw new Error("release webhook state malformed");
  }
  for (const [map, validateID] of [
    [s.ampEvents, (_id: string) => true],
    [s.githubDeliveries, (id: string) => GITHUB_DELIVERY_ID.test(id)],
  ] as const) {
    const claims = new Set<string>();
    for (const [id, value] of Object.entries(map)) {
      if (
        !id || !validateID(id) ||
        typeof value !== "string" ||
        !(value in s.releases) ||
        claims.has(id)
      )
        throw new Error("release webhook state malformed");
      claims.add(id);
    }
  }
  return structuredClone(s);
}
function plainMap(value: unknown): value is globalThis.Record<string, any> {
  return (
    !!value &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    Object.getPrototypeOf(value) === Object.prototype
  );
}
export const releaseKey = (r: VerifiedRelease) =>
  `${r.repositoryID}:${r.releaseID}:${r.tag}:${r.commit}:${r.imageDigest}`;
export function prompt(r: VerifiedRelease, event: string, source: string) {
  return `A fail-closed release.published webhook independently verified this deployment candidate.\n\nTrusted facts only:\n- Amp event: ${event}\n- repository: ${r.repository} (${r.repositoryID})\n- release: ${r.releaseID} ${r.releaseURL}\n- tag / published: ${r.tag} / ${r.publishedAt}\n- commit: ${r.commit}\n- receipt: ${r.kind}; sync ${r.syncID}; fingerprint ${r.planFingerprint}\n- image: ghcr.io/unstableneutron/cli-proxy-api-plus:${r.tag}@${r.imageDigest}\n- amd64 / arm64: ${r.architectures.amd64} / ${r.architectures.arm64}\n- workflow: ${r.workflowPath} run ${r.workflowRunID} attempt ${r.workflowRunAttempt}\n- configured failure-notification thread: ${source}\n\nOperate only on /home/thinh/CLIProxyAPI. Read the current /home/thinh/CLIProxyAPI/AGENTS.md and SETUP.md before acting. Never ship, patch, tag, release, or modify source. Use thread_interact to ${source} only for failure, blocker, approval, rollback, or issues and only if that callback is supported; routine success must not ping.\n\nUse a separate read-only source-validation checkout. Create a private gitignored 0700 deployment evidence directory below /home/thinh/.amp/private/release-published-webhook/deployments with 0600 files. Independently record each exact URL, path, and status plus production compose image, container image ID/digest, service state, logs, restarts, and resources. Before mutation, prove baseline health and immutable rollback digest/compose backup usability. Privately copy compatible config/auth; never output secrets.\n\nCreate a canary-only Compose project and network using the exact tag@digest image, a 127.0.0.1-only port, restart: no, no Watchtower, and no production bind mounts or mutation. Production must remain unmodified during canary. In the read-only checkout confirm the verified tag's peeled commit before using tools/upstream-sync-smoke. Run one recorded gate at a time: loopback health; catalog; then each required model and REST, SSE, and WebSocket where supported for openai/gpt-5.5, gpt-5.6-luna, gpt-5.6-sol, and cc/deepseek-v4-flash. Fail closed if protocol support cannot be established. Preserve JSON, event-stream, terminal, and failure diagnostics, then clean up canary.\n\nApply the documented endpoint ownership independently: CPA loopback /healthz succeeds while unsupported CPA loopback /health, /status, and /status/ return 404; CPAMP loopback and public routed /health have their own success expectations. Any externally routed health 404 in the production baseline must be preserved and attributed to its exact URL, never treated as a generic failure. After CPA restart, verify CPAMP collector and upstream health. Only after every gate passes update production to the exact tag@digest and run "docker compose up -d --no-deps cli-proxy-api" (service only). Repeat every live gate. Soak at least five meaningful minutes with separately sampled endpoint parity, model/protocol probes, errors, logs, restarts, and Docker resources. Retain only all-pass. On confirmed failure immediately restore the compose backup and immutable prior image service-only, then verify recovery against baseline. Preserve reproducible failure evidence and recovered production; do not repair tagged source or production. A repair PR/hotfix is separate.\n\nOperational override for production-compatible OAuth canaries: the earlier blanket ban on production bind mounts remains in force except when a valid gate requires the production credential store. In that case mount only that credential store read-write, allow exactly one short-lived canary replica, and keep every other production path isolated. Before starting it, arm an independent bounded TTL cleanup and install EXIT, INT, and TERM cleanup; remove the canary immediately on success, failure, or interruption, then prove no canary container, network, or mount holder remains. Stop before production mutation if the TTL or cleanup proof is unavailable. This temporary operational compromise is not a substitute for future cross-process locking. Run the full established REST, SSE, native WebSocket, compact, and V2-compaction smoke matrix before deployment.`;
}
export const issueMarker = (code: string, key: string) =>
  `[release-published-webhook issue:${code.replace(/[^a-z0-9-]/g, "-")}:${createHash("sha256").update(key).digest("hex")}]`;
export async function notifyIssue(
  thread: Thread,
  code: string,
  key: string,
): Promise<boolean> {
  const marker = issueMarker(code, key);
  if (await thread.hasMarker(marker)) return false;
  await thread.append(
    `${marker}\nDeployment dispatch requires operator review. See sanitized workspace state and plugin logs; no release content or credentials are included.`,
  );
  return true;
}
export async function dispatch(
  store: Store,
  spawner: Spawner,
  r: VerifiedRelease,
  ampID: string,
  deliveryID: string,
  source: string,
  now = () => new Date().toISOString(),
) {
  const key = releaseKey(r);
  let s = await store.load();
  for (const [map, id] of [
    [s.ampEvents, ampID],
    [s.githubDeliveries, deliveryID],
  ] as const)
    if (map[id] && map[id] !== key)
      return { outcome: "blocked" as const, issue: "identity-rebind", releaseKey: key };
  const old = s.releases[key];
  if (old) {
    s.ampEvents[ampID] = key;
    s.githubDeliveries[deliveryID] = key;
    await store.save(s);
    if (
      (old.status === "thread-created" || old.status === "append-failed") &&
      old.threadID
    ) {
      const t = spawner.get(old.threadID);
      try {
        if (!(await t.hasMarker(`- Amp event: ${old.ampEventID}`)))
          await t.append(prompt(r, old.ampEventID, source));
        old.status = "dispatched";
        old.error = undefined;
        old.updatedAt = now();
        await store.save(s);
        return { outcome: "dispatched" as const, threadID: t.id };
      } catch (e) {
        old.status = "append-failed";
        old.error = "thread-append-failed";
        old.updatedAt = now();
        await store.save(s);
        throw new Error("thread-append-failed");
      }
    }
    return {
      outcome:
        old.status === "dispatched"
          ? ("duplicate" as const)
          : ("blocked" as const),
      threadID: old.threadID,
      issue: old.status === "dispatched" ? undefined : old.status,
      releaseKey: key,
    };
  }
  if (!(await store.claim(key))) {
    return { outcome: "blocked" as const, issue: "existing-claim", releaseKey: key };
  }
  const rec: Record = {
    key,
    ampEventID: ampID,
    githubDeliveryID: deliveryID,
    status: "claimed",
    updatedAt: now(),
  };
  s.releases[key] = rec;
  s.ampEvents[ampID] = key;
  s.githubDeliveries[deliveryID] = key;
  await store.save(s);
  let t: Thread;
  try {
    t = await spawner.create();
  } catch (e) {
    s = await store.load();
    const durable = s.releases[key] ?? structuredClone(rec);
    s.releases[key] = durable;
    s.ampEvents[rec.ampEventID] = key;
    s.githubDeliveries[rec.githubDeliveryID] = key;
    durable.status = "creation-uncertain";
    durable.error = "thread-create-failed";
    durable.updatedAt = now();
    await store.save(s);
    return { outcome: "blocked" as const, issue: "creation-uncertain", releaseKey: key };
  }
  s = await store.load();
  const durable = s.releases[key] ?? structuredClone(rec);
  s.releases[key] = durable;
  s.ampEvents[rec.ampEventID] = key;
  s.githubDeliveries[rec.githubDeliveryID] = key;
  Object.assign(durable, {
    threadID: t.id,
    status: "thread-created",
    updatedAt: now(),
  });
  await store.save(s);
  try {
    await t.append(prompt(r, ampID, source));
  } catch (e) {
    s.releases[key].status = "append-failed";
    s.releases[key].error = "thread-append-failed";
    s.releases[key].updatedAt = now();
    await store.save(s);
    throw new Error("thread-append-failed");
  }
  s.releases[key].status = "dispatched";
  s.releases[key].updatedAt = now();
  await store.save(s);
  return { outcome: "dispatched" as const, threadID: t.id };
}
