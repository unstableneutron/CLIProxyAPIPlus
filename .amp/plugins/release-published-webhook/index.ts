import type { PluginAPI, PluginThread } from "@ampcode/plugin";
import { chmod, lstat, mkdir, open, readFile, writeFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { join } from "node:path";
import { admitWebhook } from "./admission";
import { dispatch, notifyIssue, parseState, type Thread } from "./dispatch";
import {
  GitHubHTTPError,
  REPOSITORY,
  RejectedDelivery,
  RetryableNotReady,
  revalidateRelease,
  validateRelease,
  type GitHubClient,
} from "./provenance";
import { PublicGhcrRegistry } from "./registry";
export const description =
  "Verifies release provenance and starts one VN3 deployment thread.";
const dir = join(".amp", "private", "release-published-webhook"),
  stateKey = "releasePublishedWebhookState";
export function githubDownloadAccept(url: string): string {
  const artifactArchive =
    /^\/repos\/unstableneutron\/CLIProxyAPIPlus\/actions\/artifacts\/[1-9][0-9]*\/zip$/.test(
      new URL(url).pathname,
    );
  return artifactArchive
    ? "application/vnd.github+json"
    : "application/octet-stream";
}
class GitHub implements GitHubClient {
  constructor(private token: string) {}
  async get(path: string, signal: AbortSignal) {
    const r = await fetch(`https://api.github.com${path}`, {
      headers: {
        Accept: "application/vnd.github+json",
        Authorization: `Bearer ${this.token}`,
        "X-GitHub-Api-Version": "2022-11-28",
      },
      signal,
    });
    if (!r.ok)
      throw new GitHubHTTPError(r.status, `GitHub API returned ${r.status}`);
    return r.json();
  }
  async bytes(url: string, signal: AbortSignal) {
    const r = await fetch(url, {
      headers: {
        Accept: githubDownloadAccept(url),
        Authorization: `Bearer ${this.token}`,
        "X-GitHub-Api-Version": "2022-11-28",
      },
      signal,
    });
    if (!r.ok)
      throw new GitHubHTTPError(r.status, `GitHub asset returned ${r.status}`);
    return new Uint8Array(await r.arrayBuffer());
  }
}
export const runnerCreateOptions = () => ({
  executor: { type: "runner" as const, id: "vn3" },
});
export const HIGH_AGENT_MODE = "high" as const;
export const UNEXPECTED_WEBHOOK_ERROR = "release webhook processing failed";
async function syncDirectory(path: string): Promise<void> {
  const directory = await open(path, "r");
  try {
    await directory.sync();
  } finally {
    await directory.close();
  }
}
export async function initializeClaimsDirectory(privateDir: string): Promise<string> {
  const claimsDir = join(privateDir, "claims");
  await mkdir(claimsDir, { recursive: true, mode: 0o700 });
  const stat = await lstat(claimsDir);
  if (!stat.isDirectory() || stat.isSymbolicLink())
    throw new Error("unsafe claims directory");
  await chmod(claimsDir, 0o700);
  await syncDirectory(privateDir);
  return claimsDir;
}
export async function atomicClaim(claimsDir: string, key: string): Promise<boolean> {
  const name = createHash("sha256").update(key).digest("hex");
  let file;
  try {
    file = await open(join(claimsDir, name), "wx", 0o600);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "EEXIST") return false;
    throw error;
  }
  try {
    await file.chmod(0o600);
    await file.sync();
  } finally {
    await file.close();
  }
  await syncDirectory(claimsDir);
  return true;
}
function adapter(t: PluginThread): Thread {
  return {
    id: t.id,
    append: (p) => t.appendUserMessage({ type: "user-message", content: p }),
    hasMarker: async (marker) =>
      (await t.messages({ full: true, from: "start", roles: ["user"] })).some(
        (m) =>
          m.content.some((b) => b.type === "text" && b.text.includes(marker)),
      ),
  };
}
let serial = Promise.resolve();
const exclusive = <T>(fn: () => Promise<T>) => {
  const p = serial.then(fn, fn);
  serial = p.then(
    () => undefined,
    () => undefined,
  );
  return p;
};
async function setting(env: string, path: string) {
  if (process.env[env] !== undefined) return process.env[env]!.trim();
  const stat = await lstat(path);
  if (!stat.isFile() || stat.isSymbolicLink()) throw new Error("unsafe private setting");
  await chmod(path, 0o600);
  return (await readFile(path, "utf8")).trim();
}
export default async function (amp: PluginAPI) {
  const uri = amp.system.workspaceRoot;
  if (!uri) {
    amp.logger.log("release webhook disabled: no workspace");
    return;
  }
  const root = amp.helpers.filePathFromURI(uri),
    privateDir = join(root, dir);
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  const privateStat = await lstat(privateDir);
  if (!privateStat.isDirectory() || privateStat.isSymbolicLink()) {
    amp.logger.log("release webhook disabled: unsafe private directory");
    return;
  }
  await chmod(privateDir, 0o700);
  let claimsDir: string;
  try {
    claimsDir = await initializeClaimsDirectory(privateDir);
  } catch {
    amp.logger.log("release webhook disabled: unsafe claims directory");
    return;
  }
  let secret: string, token: string, source: string;
  try {
    [secret, token, source] = await Promise.all([
      setting(
        "RELEASE_PUBLISHED_WEBHOOK_SECRET",
        join(privateDir, "github-webhook-secret"),
      ),
      setting(
        "RELEASE_PUBLISHED_GITHUB_TOKEN",
        join(privateDir, "github-token"),
      ),
      setting(
        "RELEASE_PUBLISHED_NOTIFICATION_THREAD_ID",
        join(privateDir, "notification-thread-id"),
      ),
    ]);
  } catch {
    amp.logger.log("release webhook disabled: private configuration missing");
    return;
  }
  if (
    secret.length < 32 ||
    !token ||
    !/^T-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(
      source,
    )
  ) {
    amp.logger.log("release webhook disabled: private configuration invalid");
    return;
  }
  const gh = new GitHub(token),
    registry = new PublicGhcrRegistry(),
    high = amp.getBuiltinAgent(HIGH_AGENT_MODE);
  const registration = await amp.createWebhook({
    key: "release-published",
    headers: ["x-github-event", "x-github-delivery", "x-hub-signature-256"],
    handler: async (event, context) =>
      exclusive(async () => {
        let verifiedKey: string | undefined;
        try {
          const admitted = admitWebhook(event.headers, event.body, secret);
          if (admitted.kind === "ignored") return;
          const release = await validateRelease(
            admitted.payload,
            event.receivedAt,
            gh,
            registry,
            context.signal,
          );
          await revalidateRelease(release, gh, registry, context.signal);
          verifiedKey = `${release.repositoryID}:${release.releaseID}:${release.tag}:${release.commit}:${release.imageDigest}`;
          const result = await dispatch(
            {
              load: async () =>
                parseState((await amp.configuration.get())[stateKey]),
              save: (s) =>
                amp.configuration.update({ [stateKey]: s }, "workspace"),
              claim: (key) => atomicClaim(claimsDir, key),
            },
            {
              create: async () =>
                adapter(await high.createThread(runnerCreateOptions())),
              get: (id) => adapter(amp.threads.get(id as `T-${string}`)),
            },
            release,
            event.id,
            admitted.deliveryID,
            source,
          );
          amp.logger.log(
            `release webhook ${result.outcome} release ${release.releaseID}`,
          );
          if (result.outcome === "blocked" && result.issue) {
            await notifyIssue(
              adapter(amp.threads.get(source as `T-${string}`)),
              result.issue,
              result.releaseKey!,
            );
          }
        } catch (e) {
          if (e instanceof RetryableNotReady) throw e;
          if (e instanceof RejectedDelivery) {
            amp.logger.log(`release webhook rejected: ${e.message}`);
            return;
          }
          if (verifiedKey) {
            try {
              await notifyIssue(
                adapter(amp.threads.get(source as `T-${string}`)),
                "unexpected-verified-failure",
                verifiedKey,
              );
            } catch {}
          }
          throw new Error(UNEXPECTED_WEBHOOK_ERROR);
        }
      }),
  });
  const urlPath = join(privateDir, "url");
  try {
    const urlStat = await lstat(urlPath);
    if (!urlStat.isFile() || urlStat.isSymbolicLink())
      throw new Error("unsafe private URL file");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  await writeFile(urlPath, `${registration.url}\n`, {
    mode: 0o600,
  });
  const writtenURL = await lstat(urlPath);
  if (!writtenURL.isFile() || writtenURL.isSymbolicLink())
    throw new Error("unsafe private URL file");
  await chmod(urlPath, 0o600);
  amp.logger.log(`release webhook registered for ${REPOSITORY}`);
}
