import { describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  BOT_ID,
  BOT_LOGIN,
  GitHubHTTPError,
  IMAGE,
  OWNER_ID,
  OWNER_LOGIN,
  REPOSITORY,
  REPOSITORY_ID,
  RejectedDelivery,
  RetryableNotReady,
  revalidateRelease,
  validateRelease,
  verifyRegistry,
  type GitHubClient,
  type RegistryClient,
  type RegistryManifest,
} from "./provenance";

const signal = new AbortController().signal;
const receivedAt = "2026-08-15T05:35:00Z";
const now = () => Date.parse("2026-08-15T05:36:00Z");
const owner = { login: OWNER_LOGIN, id: OWNER_ID, type: "User" };
const bot = { login: BOT_LOGIN, id: BOT_ID, type: "Bot" };
const repository = { full_name: REPOSITORY, id: REPOSITORY_ID, owner };
const indexMediaType = "application/vnd.oci.image.index.v1+json";
const manifestMediaType = "application/vnd.oci.image.manifest.v1+json";

function bytes(value: string): Uint8Array {
  return new TextEncoder().encode(value);
}

function sha256(value: Uint8Array): string {
  return `sha256:${createHash("sha256").update(value).digest("hex")}`;
}

function rawSHA256(value: Uint8Array): string {
  return sha256(value).slice("sha256:".length);
}

function crc32(value: Uint8Array) { let c=0xffffffff; for(const x of value){c^=x; for(let i=0;i<8;i++) c=(c>>>1)^(0xedb88320&-(c&1));} return (c^0xffffffff)>>>0; }
function storedZip(files: Record<string, Uint8Array>): Uint8Array {
  const locals: Buffer[] = [], centrals: Buffer[] = []; let offset=0;
  for (const [name,value] of Object.entries(files)) { const n=Buffer.from(name), data=Buffer.from(value), crc=crc32(value); const l=Buffer.alloc(30+n.length); l.writeUInt32LE(0x04034b50); l.writeUInt16LE(20,4); l.writeUInt16LE(8,6); l.writeUInt16LE(n.length,26); n.copy(l,30); const descriptor=Buffer.alloc(16); descriptor.writeUInt32LE(0x08074b50); descriptor.writeUInt32LE(crc,4); descriptor.writeUInt32LE(data.length,8); descriptor.writeUInt32LE(data.length,12); locals.push(l,data,descriptor); const c=Buffer.alloc(46+n.length); c.writeUInt32LE(0x02014b50); c.writeUInt16LE(20,4); c.writeUInt16LE(20,6); c.writeUInt16LE(8,8); c.writeUInt32LE(crc,16); c.writeUInt32LE(data.length,20); c.writeUInt32LE(data.length,24); c.writeUInt16LE(n.length,28); c.writeUInt32LE(offset,42); n.copy(c,46); centrals.push(c); offset+=l.length+data.length+descriptor.length; }
  const central=Buffer.concat(centrals), end=Buffer.alloc(22); end.writeUInt32LE(0x06054b50); end.writeUInt16LE(centrals.length,8); end.writeUInt16LE(centrals.length,10); end.writeUInt32LE(central.length,12); end.writeUInt32LE(offset,16); return new Uint8Array(Buffer.concat([...locals,central,end]));
}

function fixturePlanFingerprint(tag: string, commit: string): string {
  const input = [
    `base_fork_commit=${commit}`,
    "original_tag=v7.2.132",
    `original_commit=${"2".repeat(40)}`,
    "plus_tag=v7.2.127-3",
    `plus_tag_commit=${"3".repeat(40)}`,
    `plus_head_commit=${"3".repeat(40)}`,
    "plus_head_included=false",
    `models_commit=${"4".repeat(40)}`,
    `expected_fork_tag=${tag}`,
    "",
  ].join("\n");
  const body = Buffer.from(input);
  return createHash("sha1")
    .update(`blob ${body.length}\0`)
    .update(body)
    .digest("hex");
}

function runState(receipt: Record<string,any>, commit:string, tag:string) { return bytes(JSON.stringify({schema_version:1,state:"released",target:{base_fork_commit:"1".repeat(40),original:{tag:"v7.2.132",commit:"2".repeat(40)},plus:{tag:"v7.2.127-3",tag_commit:"3".repeat(40),head:"3".repeat(40),head_included:false},models_commit:"4".repeat(40),sync_id:receipt.sync_id,plan_fingerprint:receipt.plan_fingerprint,expected_fork_tag:tag,target_drift:true,blocked:false},candidate:{branch:`upstream-sync/${receipt.sync_id}-${receipt.plan_fingerprint.slice(0,12)}`,sha:commit,acceptable:true,validation_status:"passed"},repair:{imported:false,pr:null,sha:null},final_plan:{status:"clean-noop",plan_fingerprint:fixturePlanFingerprint(tag,commit),has_changes:false,target_drift:false,blocked:false},runtime_smoke:"not_run",vn3_deployed:false,promotion:{commit,tag},release:{url:receipt.release_url,assets:receipt.release_assets,image:receipt.image,image_digest:receipt.image_digest,platforms:receipt.platforms,architecture_images:receipt.architecture_images}})); }

function finalPlan(tag: string, commit: string) {
  const tagParts = /^(.*)\.([0-9]+)$/.exec(tag)!;
  const syncID = "original-v7.2.132_plus-v7.2.127-3";
  const fingerprint = fixturePlanFingerprint(tag, commit);
  const namespace = `refs/upstream-sync/${fingerprint}`;
  const values: Record<string, string> = {
    original_tag: "v7.2.132",
    plus_tag: "v7.2.127-3",
    pre_sync_head: commit,
    base_fork_commit: commit,
    original_repository: "router-for-me/CLIProxyAPI",
    plus_repository: "kaitranntt/CLIProxyAPIPlus",
    models_repository: "router-for-me/models",
    original_head: "2".repeat(40),
    plus_tag_head: "3".repeat(40),
    plus_head: "3".repeat(40),
    models_commit: "4".repeat(40),
    plus_head_included: "false",
    plus_head_already_represented: "true",
    plus_head_delta_paths: "",
    unsafe_plus_head_delta_paths: "",
    blocked: "false",
    block_reason: "",
    fork_tag_prefix: tagParts[1],
    latest_fork_tag: tag,
    latest_fork_models_commit: "4".repeat(40),
    latest_fork_suffix: String(Number(tagParts[2])),
    next_fork_tag: `${tagParts[1]}.${Number(tagParts[2]) + 1}`,
    expected_fork_tag: tag,
    safe_sync_id: syncID,
    plan_fingerprint: fingerprint,
    candidate_branch: `upstream-sync/${syncID}-${fingerprint.slice(0, 12)}`,
    snapshot_namespace: namespace,
    original_snapshot_ref: `${namespace}/original`,
    plus_tag_snapshot_ref: `${namespace}/plus-tag`,
    plus_head_snapshot_ref: `${namespace}/plus-head`,
    models_snapshot_ref: `${namespace}/models`,
    target_drift: "false",
    target_drift_summary: "",
    has_changes: "false",
  };
  return bytes(
    `${Object.entries(values)
      .map(([key, value]) => `${key}=${value}`)
      .join("\n")}\n`,
  );
}

function encodedContent(value: Uint8Array) {
  return {
    encoding: "base64",
    size: value.length,
    content: Buffer.from(value).toString("base64"),
  };
}

interface RegistryFixture {
  client: RegistryClient;
  manifests: Map<string, RegistryManifest>;
  index: Record<string, unknown>;
  architectureImages: Record<string, { image: string; digest: string }>;
  calls: string[];
  refreshIndex(): string;
}

function registryFixture(tag: string): RegistryFixture {
  const calls: string[] = [];
  const amd64Body = bytes(
    JSON.stringify({
      schemaVersion: 2,
      mediaType: manifestMediaType,
      config: {},
    }),
  );
  const arm64Body = bytes(
    JSON.stringify({
      schemaVersion: 2,
      mediaType: manifestMediaType,
      config: { arm: true },
    }),
  );
  const amd64Digest = sha256(amd64Body);
  const arm64Digest = sha256(arm64Body);
  const index: Record<string, unknown> = {
    schemaVersion: 2,
    mediaType: indexMediaType,
    manifests: [
      {
        digest: amd64Digest,
        mediaType: manifestMediaType,
        platform: { os: "linux", architecture: "amd64" },
      },
      {
        digest: `sha256:${"c".repeat(64)}`,
        mediaType: manifestMediaType,
        annotations: {
          "vnd.docker.reference.digest": amd64Digest,
          "vnd.docker.reference.type": "attestation-manifest",
        },
        platform: { os: "unknown", architecture: "unknown" },
      },
      {
        digest: arm64Digest,
        mediaType: manifestMediaType,
        platform: { os: "linux", architecture: "arm64" },
      },
      {
        digest: `sha256:${"d".repeat(64)}`,
        mediaType: manifestMediaType,
        annotations: {
          "vnd.docker.reference.digest": arm64Digest,
          "vnd.docker.reference.type": "attestation-manifest",
        },
        platform: { os: "unknown", architecture: "unknown" },
      },
    ],
  };
  const manifests = new Map<string, RegistryManifest>([
    [
      `${tag}-amd64`,
      { bytes: amd64Body, digest: amd64Digest, mediaType: manifestMediaType },
    ],
    [
      `${tag}-arm64`,
      { bytes: arm64Body, digest: arm64Digest, mediaType: manifestMediaType },
    ],
  ]);
  const fixture: RegistryFixture = {
    manifests,
    index,
    architectureImages: {
      "linux/amd64": { image: `${IMAGE}:${tag}-amd64`, digest: amd64Digest },
      "linux/arm64": { image: `${IMAGE}:${tag}-arm64`, digest: arm64Digest },
    },
    calls,
    client: {
      manifest: async (reference) => {
        calls.push(reference);
        const manifest = manifests.get(reference);
        if (!manifest) throw new Error("missing registry fixture");
        return structuredClone(manifest);
      },
    },
    refreshIndex() {
      const body = bytes(JSON.stringify(index));
      const digest = sha256(body);
      manifests.set(tag, { bytes: body, digest, mediaType: indexMediaType });
      manifests.set("latest", {
        bytes: body,
        digest,
        mediaType: indexMediaType,
      });
      return digest;
    },
  };
  fixture.refreshIndex();
  return fixture;
}

interface ReleaseFixture {
  payload: Record<string, any>;
  values: Map<string, any>;
  assetBytes: Map<string, Uint8Array>;
  github: GitHubClient;
  registry: RegistryFixture;
  canonical: Record<string, any>;
  receipt: Record<string, any>;
  receiptAsset: Record<string, any>;
  checksumAsset: Record<string, any>;
  archiveAsset: Record<string, any>;
  currentTag: string;
  currentCommit: string;
  currentWorkflowID: number;
  stateBytes: Uint8Array;
  baseReceipt?: Record<string, any>;
  baseReceiptAsset?: Record<string, any>;
  baseRelease?: Record<string, any>;
  baseCommit?: string;
  baseTag?: string;
  refreshReceipt(): void;
  refreshChecksum(content: string): void;
  refreshBaseReceipt?(): void;
}

function makeAsset(
  id: number,
  name: string,
  content: Uint8Array,
  assetBytes: Map<string, Uint8Array>,
): Record<string, any> {
  const url = `https://api.github.com/repos/${REPOSITORY}/releases/assets/${id}`;
  assetBytes.set(url, content);
  return {
    id,
    name,
    url,
    size: content.length,
    state: "uploaded",
    digest: sha256(content),
    uploader: structuredClone(bot),
  };
}

function stateFile(expectedTag: string): Uint8Array {
  const fingerprint = "14e308bd7b8de821e7d600e938543cbec944f6fb";
  const syncID = "original-v7.2.132_plus-v7.2.127-3";
  return bytes(
    [
      "SCHEMA_VERSION=2",
      `SYNC_ID=${syncID}`,
      `PLAN_FINGERPRINT=${fingerprint}`,
      `BASE_FORK_COMMIT=${"1".repeat(40)}`,
      "ORIGINAL_REPOSITORY=router-for-me/CLIProxyAPI",
      "ORIGINAL_TAG=v7.2.132",
      `ORIGINAL_COMMIT=${"2".repeat(40)}`,
      "PLUS_REPOSITORY=kaitranntt/CLIProxyAPIPlus",
      "PLUS_TAG=v7.2.127-3",
      `PLUS_TAG_COMMIT=${"3".repeat(40)}`,
      `PLUS_HEAD_COMMIT=${"3".repeat(40)}`,
      "PLUS_HEAD_INCLUDED=false",
      "MODELS_REPOSITORY=router-for-me/models",
      `MODELS_COMMIT=${"4".repeat(40)}`,
      `EXPECTED_FORK_TAG=${expectedTag}`,
      `CANDIDATE_BRANCH=upstream-sync/${syncID}-${fingerprint.slice(0, 12)}`,
      "",
    ].join("\n"),
  );
}

function releaseFixture(
  kind: "upstream" | "hotfix" = "upstream",
): ReleaseFixture {
  const hotfix = kind === "hotfix";
  const baseTag = "v7.2.132-unstableneutron.0";
  const currentTag = hotfix ? "v7.2.132-unstableneutron.1" : baseTag;
  const baseCommit = "5".repeat(40);
  const currentCommit = hotfix ? "6".repeat(40) : baseCommit;
  const currentTagObject = hotfix ? "7".repeat(40) : "8".repeat(40);
  const baseTagObject = "8".repeat(40);
  const currentWorkflowID = hotfix ? 900 : 800;
  const baseWorkflowID = 800;
  const releaseID = hotfix ? 101 : 100;
  const publishedAt = "2026-08-15T05:32:46Z";
  const assetBytes = new Map<string, Uint8Array>();
  const values = new Map<string, any>();
  const registry = registryFixture(currentTag);
  const baseRegistry = hotfix ? registryFixture(baseTag) : registry;
  if (hotfix) {
    for (const [reference, manifest] of baseRegistry.manifests) {
      if (reference !== "latest") registry.manifests.set(reference, manifest);
    }
  }
  const archiveName = `CLIProxyAPIPlus_7.2.132-unstableneutron.${hotfix ? 1 : 0}_linux_amd64_no-plugin.tar.gz`;
  const archiveContent = bytes(`archive-${kind}`);
  const archiveAsset = makeAsset(11, archiveName, archiveContent, assetBytes);
  const checksumContent = `${rawSHA256(archiveContent)}  ${archiveName}\n`;
  const checksumAsset = makeAsset(
    12,
    "checksums.txt",
    bytes(checksumContent),
    assetBytes,
  );
  const releaseURL = `https://github.com/${REPOSITORY}/releases/tag/${currentTag}`;
  const architectureImages = structuredClone(registry.architectureImages);
  const receipt: Record<string, any> = {
    schema_version: 2,
    sync_id: "original-v7.2.132_plus-v7.2.127-3",
    plan_fingerprint: "14e308bd7b8de821e7d600e938543cbec944f6fb",
    main_commit: currentCommit,
    tag: currentTag,
    tag_commit: currentCommit,
    release_url: releaseURL,
    release_assets: [archiveName, "checksums.txt"].sort(),
    image: `${IMAGE}:${currentTag}`,
    image_digest: registry.manifests.get(currentTag)!.digest,
    platforms: ["linux/amd64", "linux/arm64"],
    workflow_run_id: String(currentWorkflowID),
    architecture_images: architectureImages,
  };
  const stateBytes = stateFile(baseTag);
  if (hotfix) {
    Object.assign(receipt, {
      receipt_type: "hotfix-release",
      hotfix_schema_version: 1,
      previous_release: { tag: baseTag, commit: baseCommit },
      upstream_state: {
        sync_id: receipt.sync_id,
        plan_fingerprint: receipt.plan_fingerprint,
        sha256: rawSHA256(stateBytes),
      },
      release_asset_digests: {
        [archiveName]: archiveAsset.digest,
        "checksums.txt": checksumAsset.digest,
      },
      release_workflow: {
        path: ".github/workflows/hotfix-release.yml",
        ref: `${REPOSITORY}/.github/workflows/hotfix-release.yml@refs/heads/main`,
        commit: currentCommit,
        run_id: String(currentWorkflowID),
        run_attempt: "1",
      },
    });
  }
  const receiptName = hotfix
    ? "hotfix-release-receipt.json"
    : "upstream-sync-receipt.json";
  const receiptAsset = makeAsset(
    13,
    receiptName,
    bytes(JSON.stringify(receipt)),
    assetBytes,
  );
  const canonical: Record<string, any> = {
    id: releaseID,
    tag_name: currentTag,
    html_url: releaseURL,
    assets_url: `https://api.github.com/repos/${REPOSITORY}/releases/${releaseID}/assets`,
    published_at: publishedAt,
    draft: false,
    prerelease: false,
    target_commitish: "main",
    author: structuredClone(bot),
    assets: [checksumAsset, archiveAsset, receiptAsset],
  };
  const payload = {
    action: "published",
    repository: structuredClone(repository),
    sender: structuredClone(bot),
    release: {
      id: releaseID,
      tag_name: currentTag,
      html_url: canonical.html_url,
      assets_url: canonical.assets_url,
      published_at: publishedAt,
      draft: false,
      prerelease: false,
      target_commitish: "main",
      author: structuredClone(bot),
      body: "SENTINEL UNTRUSTED RELEASE TEXT",
    },
  };

  values.set(`/repos/${REPOSITORY}`, {
    ...structuredClone(repository),
    default_branch: "main",
  });
  values.set(`/repos/${REPOSITORY}/releases/${releaseID}`, canonical);
  values.set(
    `/repos/${REPOSITORY}/releases/tags/${currentTag}`,
    structuredClone(canonical),
  );
  values.set(`/repos/${REPOSITORY}/releases/latest`, {
    id: releaseID,
    tag_name: currentTag,
  });
  values.set(`/repos/${REPOSITORY}/git/ref/tags/${currentTag}`, {
    ref: `refs/tags/${currentTag}`,
    object: { type: "tag", sha: currentTagObject },
  });
  values.set(`/repos/${REPOSITORY}/git/tags/${currentTagObject}`, {
    sha: currentTagObject,
    tag: currentTag,
    object: { type: "commit", sha: currentCommit },
    tagger: hotfix
      ? {
          name: "cliproxy-hotfix-release[bot]",
          email: "cliproxy-hotfix-release@users.noreply.github.com",
          date: publishedAt,
        }
      : {
          name: "cliproxy-upstream-sync[bot]",
          email: "cliproxy-upstream-sync@users.noreply.github.com",
          date: publishedAt,
        },
    message: hotfix
      ? `Hotfix release ${currentTag} after ${baseTag}\n`
      : `Release ${currentTag}\n`,
  });
  values.set(`/repos/${REPOSITORY}/commits/${currentCommit}`, {
    sha: currentCommit,
  });
  values.set(`/repos/${REPOSITORY}/commits/main`, { sha: currentCommit });
  values.set(`/repos/${REPOSITORY}/compare/${currentCommit}...main`, {
    status: "identical",
  });
  values.set(
    `/repos/${REPOSITORY}/contents/.ccs-fork-upstream.env?ref=${currentCommit}`,
    encodedContent(stateBytes),
  );
  values.set(`/repos/${REPOSITORY}/actions/runs/${currentWorkflowID}`, {
    repository: structuredClone(repository),
    actor: structuredClone(owner),
    path: hotfix
      ? ".github/workflows/hotfix-release.yml"
      : ".github/workflows/upstream-sync-v2.yml",
    head_branch: "main",
    head_sha: hotfix ? currentCommit : "9".repeat(40),
    status: "completed",
    conclusion: "success",
    event: "workflow_dispatch",
    run_attempt: 1,
  });
  if (!hotfix) {
    values.set(
      `/repos/${REPOSITORY}/compare/${"9".repeat(40)}...${currentCommit}`,
      { status: "ahead" },
    );
  }

  let baseReceipt: Record<string, any> | undefined;
  let baseReceiptAsset: Record<string, any> | undefined;
  let baseRelease: Record<string, any> | undefined;
  if (hotfix) {
    values.set(
      `/repos/${REPOSITORY}/compare/${baseCommit}...${currentCommit}`,
      { status: "ahead" },
    );
    const baseArchiveName =
      "CLIProxyAPIPlus_7.2.132-unstableneutron.0_linux_amd64_no-plugin.tar.gz";
    const baseArchiveContent = bytes("base-archive");
    const baseArchive = makeAsset(
      21,
      baseArchiveName,
      baseArchiveContent,
      assetBytes,
    );
    const baseChecksum = makeAsset(
      22,
      "checksums.txt",
      bytes(`${rawSHA256(baseArchiveContent)}  ${baseArchiveName}\n`),
      assetBytes,
    );
    const baseURL = `https://github.com/${REPOSITORY}/releases/tag/${baseTag}`;
    baseReceipt = {
      schema_version: 2,
      sync_id: receipt.sync_id,
      plan_fingerprint: receipt.plan_fingerprint,
      main_commit: baseCommit,
      tag: baseTag,
      tag_commit: baseCommit,
      release_url: baseURL,
      release_assets: [baseArchiveName, "checksums.txt"].sort(),
      image: `${IMAGE}:${baseTag}`,
      image_digest: baseRegistry.manifests.get(baseTag)!.digest,
      platforms: ["linux/amd64", "linux/arm64"],
      workflow_run_id: String(baseWorkflowID),
      architecture_images: structuredClone(baseRegistry.architectureImages),
    };
    baseReceiptAsset = makeAsset(
      23,
      "upstream-sync-receipt.json",
      bytes(JSON.stringify(baseReceipt)),
      assetBytes,
    );
    baseRelease = {
      id: 100,
      tag_name: baseTag,
      html_url: baseURL,
      assets_url: `https://api.github.com/repos/${REPOSITORY}/releases/100/assets`,
      published_at: "2026-08-14T05:32:46Z",
      draft: false,
      prerelease: false,
      target_commitish: "main",
      author: structuredClone(bot),
      assets: [baseChecksum, baseArchive, baseReceiptAsset],
    };
    values.set(
      `/repos/${REPOSITORY}/releases/tags/${baseTag}`,
      structuredClone(baseRelease),
    );
    values.set(`/repos/${REPOSITORY}/releases/100`, baseRelease);
    values.set(`/repos/${REPOSITORY}/git/ref/tags/${baseTag}`, {
      ref: `refs/tags/${baseTag}`,
      object: { type: "tag", sha: baseTagObject },
    });
    values.set(`/repos/${REPOSITORY}/git/tags/${baseTagObject}`, {
      sha: baseTagObject,
      tag: baseTag,
      object: { type: "commit", sha: baseCommit },
      tagger: {
        name: "cliproxy-upstream-sync[bot]",
        email: "cliproxy-upstream-sync@users.noreply.github.com",
        date: "2026-08-14T05:30:00Z",
      },
      message: `Release ${baseTag}\n`,
    });
    values.set(
      `/repos/${REPOSITORY}/contents/.ccs-fork-upstream.env?ref=${baseCommit}`,
      encodedContent(stateBytes),
    );
    values.set(`/repos/${REPOSITORY}/actions/runs/${baseWorkflowID}`, {
      repository: structuredClone(repository),
      actor: structuredClone(owner),
      path: ".github/workflows/upstream-sync-v2.yml",
      head_branch: "main",
      head_sha: "a".repeat(40),
      status: "completed",
      conclusion: "success",
      event: "schedule",
      run_attempt: 1,
    });
    values.set(
      `/repos/${REPOSITORY}/compare/${"a".repeat(40)}...${baseCommit}`,
      { status: "ahead" },
    );
  }

  const addArtifact = (runID:number, artifactKind:"upstream"|"hotfix", artifactReceipt:Record<string,any>, artifactCommit:string, artifactTag:string, runHead:string) => {
    const receiptFile = bytes(JSON.stringify(artifactReceipt));
    const files = artifactKind === "upstream" ? {"nested/upstream-sync-receipt.json":receiptFile,"work/run-state.json":runState(artifactReceipt,artifactCommit,artifactTag)} : {"hotfix-release-receipt.json":receiptFile,"verify/independently-verified-receipt.json":receiptFile,"final-plan.out":finalPlan(artifactTag,artifactCommit)};
    const zip=storedZip(files), id=runID+10000, url=`https://api.github.com/repos/${REPOSITORY}/actions/artifacts/${id}/zip`;
    assetBytes.set(url,zip);
    values.set(`/repos/${REPOSITORY}/actions/runs/${runID}/artifacts?per_page=100`,{total_count:1,artifacts:[{id,name:`${artifactKind === "upstream" ? "upstream-sync" : "hotfix-release"}-receipt-${runID}-1`,digest:sha256(zip),size_in_bytes:zip.length,expired:false,archive_download_url:url,workflow_run:{id:runID,repository_id:REPOSITORY_ID,head_repository_id:REPOSITORY_ID,head_sha:runHead}}]});
  };
  addArtifact(currentWorkflowID,kind,receipt,currentCommit,currentTag,hotfix ? currentCommit : "9".repeat(40));
  if (hotfix) addArtifact(baseWorkflowID,"upstream",baseReceipt!,baseCommit,baseTag,"a".repeat(40));

  const github: GitHubClient = {
    get: async (path) => {
      if (!values.has(path)) throw new Error(`missing GitHub fixture: ${path}`);
      return structuredClone(values.get(path));
    },
    bytes: async (url) => {
      const content = assetBytes.get(url);
      if (!content) throw new Error("missing asset fixture");
      return new Uint8Array(content);
    },
  };

  const fixture: ReleaseFixture = {
    payload,
    values,
    assetBytes,
    github,
    registry,
    canonical,
    receipt,
    receiptAsset,
    checksumAsset,
    archiveAsset,
    currentTag,
    currentCommit,
    currentWorkflowID,
    stateBytes,
    baseReceipt,
    baseReceiptAsset,
    baseRelease,
    baseCommit: hotfix ? baseCommit : undefined,
    baseTag: hotfix ? baseTag : undefined,
    refreshReceipt() {
      const content = bytes(JSON.stringify(receipt));
      receiptAsset.size = content.length;
      receiptAsset.digest = sha256(content);
      assetBytes.set(receiptAsset.url, content);
    },
    refreshChecksum(content: string) {
      const value = bytes(content);
      checksumAsset.size = value.length;
      checksumAsset.digest = sha256(value);
      assetBytes.set(checksumAsset.url, value);
    },
    refreshBaseReceipt: hotfix
      ? () => {
          const content = bytes(JSON.stringify(baseReceipt));
          baseReceiptAsset!.size = content.length;
          baseReceiptAsset!.digest = sha256(content);
          const taggedReceipt = values
            .get(`/repos/${REPOSITORY}/releases/tags/${baseTag}`)
            .assets.find(
              (asset: any) => asset.name === "upstream-sync-receipt.json",
            );
          taggedReceipt.size = content.length;
          taggedReceipt.digest = baseReceiptAsset!.digest;
          assetBytes.set(baseReceiptAsset!.url, content);
        }
      : undefined,
  };
  return fixture;
}

function fixtureReleaseLink(
  fixture: ReleaseFixture,
  kind: "upstream" | "hotfix",
  tag: string,
  commit: string,
  receiptAsset: Record<string, any>,
  workflowID: number,
) {
  const run = fixture.values.get(
    `/repos/${REPOSITORY}/actions/runs/${workflowID}`,
  );
  const listing = fixture.values.get(
    `/repos/${REPOSITORY}/actions/runs/${workflowID}/artifacts?per_page=100`,
  );
  const artifact = listing.artifacts.find((value: any) =>
    value.name.startsWith(
      kind === "upstream" ? "upstream-sync-receipt-" : "hotfix-release-receipt-",
    ),
  );
  const attempt = String(run.run_attempt);
  return {
    tag,
    commit,
    receipt: {
      name:
        kind === "upstream"
          ? "upstream-sync-receipt.json"
          : "hotfix-release-receipt.json",
      asset_id: String(receiptAsset.id),
      digest: receiptAsset.digest,
    },
    workflow: {
      path:
        kind === "upstream"
          ? ".github/workflows/upstream-sync-v2.yml"
          : ".github/workflows/hotfix-release.yml",
      run_id: String(workflowID),
      run_attempt: attempt,
      head_sha: run.head_sha,
    },
    artifact: {
      id: String(artifact.id),
      name: artifact.name,
      digest: artifact.digest,
    },
  };
}

function advanceHotfixFixture(fixture: ReleaseFixture): ReleaseFixture {
  const previousTag = fixture.currentTag;
  const previousCommit = fixture.currentCommit;
  const previousReceiptAsset = fixture.receiptAsset;
  const previousWorkflowID = fixture.currentWorkflowID;
  const previousSuffix = Number(previousTag.slice(previousTag.lastIndexOf(".") + 1));
  const suffix = previousSuffix + 1;
  const tag = previousTag.slice(0, previousTag.lastIndexOf(".") + 1) + suffix;
  const commit = createHash("sha1").update(`hotfix-commit-${suffix}`).digest("hex");
  const tagObject = createHash("sha1")
    .update(`hotfix-tag-${suffix}`)
    .digest("hex");
  const workflowID = 900 + suffix;
  const releaseID = 100 + suffix;
  const assetBase = 30 + suffix * 10;
  const publishedAt = "2026-08-15T05:32:46Z";
  const registry = registryFixture(tag);
  for (const [reference, manifest] of registry.manifests) {
    fixture.registry.manifests.set(reference, manifest);
  }

  const previous = fixtureReleaseLink(
    fixture,
    "hotfix",
    previousTag,
    previousCommit,
    previousReceiptAsset,
    previousWorkflowID,
  );
  const root =
    fixture.receipt.hotfix_schema_version === 2
      ? structuredClone(fixture.receipt.accepted_upstream_root)
      : fixtureReleaseLink(
          fixture,
          "upstream",
          fixture.baseTag!,
          fixture.baseCommit!,
          fixture.baseReceiptAsset!,
          800,
        );
  const archiveName = `CLIProxyAPIPlus_7.2.132-unstableneutron.${suffix}_linux_amd64_no-plugin.tar.gz`;
  const archiveContent = bytes(`archive-hotfix-${suffix}`);
  const archiveAsset = makeAsset(
    assetBase + 1,
    archiveName,
    archiveContent,
    fixture.assetBytes,
  );
  const checksumAsset = makeAsset(
    assetBase + 2,
    "checksums.txt",
    bytes(`${rawSHA256(archiveContent)}  ${archiveName}\n`),
    fixture.assetBytes,
  );
  const releaseURL = `https://github.com/${REPOSITORY}/releases/tag/${tag}`;
  const receipt: Record<string, any> = {
    schema_version: 2,
    sync_id: fixture.receipt.sync_id,
    plan_fingerprint: fixture.receipt.plan_fingerprint,
    main_commit: commit,
    tag,
    tag_commit: commit,
    release_url: releaseURL,
    release_assets: [archiveName, "checksums.txt"].sort(),
    image: `${IMAGE}:${tag}`,
    image_digest: registry.manifests.get(tag)!.digest,
    platforms: ["linux/amd64", "linux/arm64"],
    workflow_run_id: String(workflowID),
    architecture_images: structuredClone(registry.architectureImages),
    receipt_type: "hotfix-release",
    hotfix_schema_version: 2,
    previous_release: previous,
    accepted_upstream_root: root,
    upstream_state: structuredClone(fixture.receipt.upstream_state),
    release_asset_digests: {
      [archiveName]: archiveAsset.digest,
      "checksums.txt": checksumAsset.digest,
    },
    release_workflow: {
      path: ".github/workflows/hotfix-release.yml",
      ref: `${REPOSITORY}/.github/workflows/hotfix-release.yml@refs/heads/main`,
      commit,
      run_id: String(workflowID),
      run_attempt: "1",
    },
  };
  const receiptAsset = makeAsset(
    assetBase + 3,
    "hotfix-release-receipt.json",
    bytes(JSON.stringify(receipt)),
    fixture.assetBytes,
  );
  const canonical: Record<string, any> = {
    id: releaseID,
    tag_name: tag,
    html_url: releaseURL,
    assets_url: `https://api.github.com/repos/${REPOSITORY}/releases/${releaseID}/assets`,
    published_at: publishedAt,
    draft: false,
    prerelease: false,
    target_commitish: "main",
    author: structuredClone(bot),
    assets: [checksumAsset, archiveAsset, receiptAsset],
  };
  fixture.values.set(`/repos/${REPOSITORY}/releases/${releaseID}`, canonical);
  fixture.values.set(
    `/repos/${REPOSITORY}/releases/tags/${tag}`,
    structuredClone(canonical),
  );
  fixture.values.set(`/repos/${REPOSITORY}/releases/latest`, {
    id: releaseID,
    tag_name: tag,
  });
  fixture.values.set(`/repos/${REPOSITORY}/git/ref/tags/${tag}`, {
    ref: `refs/tags/${tag}`,
    object: { type: "tag", sha: tagObject },
  });
  fixture.values.set(`/repos/${REPOSITORY}/git/tags/${tagObject}`, {
    sha: tagObject,
    tag,
    object: { type: "commit", sha: commit },
    tagger: {
      name: "cliproxy-hotfix-release[bot]",
      email: "cliproxy-hotfix-release@users.noreply.github.com",
      date: publishedAt,
    },
    message: `Hotfix release ${tag} after ${previousTag}\n`,
  });
  fixture.values.set(`/repos/${REPOSITORY}/commits/${commit}`, { sha: commit });
  fixture.values.set(`/repos/${REPOSITORY}/commits/main`, { sha: commit });
  fixture.values.set(`/repos/${REPOSITORY}/compare/${commit}...main`, {
    status: "identical",
  });
  fixture.values.set(
    `/repos/${REPOSITORY}/compare/${previousCommit}...${commit}`,
    { status: "ahead" },
  );
  fixture.values.set(
    `/repos/${REPOSITORY}/contents/.ccs-fork-upstream.env?ref=${commit}`,
    encodedContent(fixture.stateBytes),
  );
  fixture.values.set(`/repos/${REPOSITORY}/actions/runs/${workflowID}`, {
    repository: structuredClone(repository),
    actor: structuredClone(owner),
    path: ".github/workflows/hotfix-release.yml",
    head_branch: "main",
    head_sha: commit,
    status: "completed",
    conclusion: "success",
    event: "workflow_dispatch",
    run_attempt: 1,
  });

  const refreshReceipt = () => {
    const content = bytes(JSON.stringify(receipt));
    receiptAsset.size = content.length;
    receiptAsset.digest = sha256(content);
    fixture.assetBytes.set(receiptAsset.url, content);
    const zip = storedZip({
      "hotfix-release-receipt.json": content,
      "independently-verified-receipt.json": content,
      "final-plan.out": finalPlan(tag, commit),
    });
    const listing = fixture.values.get(
      `/repos/${REPOSITORY}/actions/runs/${workflowID}/artifacts?per_page=100`,
    );
    const artifact = listing.artifacts[0];
    artifact.size_in_bytes = zip.length;
    artifact.digest = sha256(zip);
    fixture.assetBytes.set(artifact.archive_download_url, zip);
  };
  const initialReceipt = bytes(JSON.stringify(receipt));
  const zip = storedZip({
    "hotfix-release-receipt.json": initialReceipt,
    "independently-verified-receipt.json": initialReceipt,
    "final-plan.out": finalPlan(tag, commit),
  });
  const artifactID = workflowID + 10000;
  const artifactURL = `https://api.github.com/repos/${REPOSITORY}/actions/artifacts/${artifactID}/zip`;
  fixture.assetBytes.set(artifactURL, zip);
  fixture.values.set(
    `/repos/${REPOSITORY}/actions/runs/${workflowID}/artifacts?per_page=100`,
    {
      total_count: 1,
      artifacts: [
        {
          id: artifactID,
          name: `hotfix-release-receipt-${workflowID}-1`,
          digest: sha256(zip),
          size_in_bytes: zip.length,
          expired: false,
          archive_download_url: artifactURL,
          workflow_run: {
            id: workflowID,
            repository_id: REPOSITORY_ID,
            head_repository_id: REPOSITORY_ID,
            head_sha: commit,
          },
        },
      ],
    },
  );
  fixture.payload.release = {
    ...fixture.payload.release,
    id: releaseID,
    tag_name: tag,
    html_url: releaseURL,
    assets_url: canonical.assets_url,
    published_at: publishedAt,
  };
  Object.assign(fixture, {
    canonical,
    receipt,
    receiptAsset,
    checksumAsset,
    archiveAsset,
    currentTag: tag,
    currentCommit: commit,
    currentWorkflowID: workflowID,
    refreshReceipt,
  });
  return fixture;
}

function validate(fixture: ReleaseFixture, options = { now }) {
  return validateRelease(
    fixture.payload,
    receivedAt,
    fixture.github,
    fixture.registry.client,
    signal,
    options,
  );
}

function replaceHotfixFinalPlan(fixture: ReleaseFixture, plan: Uint8Array) {
  const listing = fixture.values.get(
    `/repos/${REPOSITORY}/actions/runs/${fixture.currentWorkflowID}/artifacts?per_page=100`,
  );
  const artifact = listing.artifacts[0];
  const receipt = fixture.assetBytes.get(fixture.receiptAsset.url)!;
  const zip = storedZip({
    "hotfix-release-receipt.json": receipt,
    "independently-verified-receipt.json": receipt,
    "final-plan.out": plan,
  });
  artifact.size_in_bytes = zip.length;
  artifact.digest = sha256(zip);
  fixture.assetBytes.set(artifact.archive_download_url, zip);
}

function replaceUpstreamRunState(fixture: ReleaseFixture, state: Uint8Array) {
  const runID = 800;
  const listing = fixture.values.get(
    `/repos/${REPOSITORY}/actions/runs/${runID}/artifacts?per_page=100`,
  );
  const artifact = listing.artifacts[0];
  const receiptAsset = fixture.baseReceiptAsset ?? fixture.receiptAsset;
  const receipt = fixture.assetBytes.get(receiptAsset.url)!;
  const zip = storedZip({
    "nested/upstream-sync-receipt.json": receipt,
    "work/run-state.json": state,
  });
  artifact.size_in_bytes = zip.length;
  artifact.digest = sha256(zip);
  fixture.assetBytes.set(artifact.archive_download_url, zip);
}

describe("upstream release provenance", () => {
  test("accepts an exact verified upstream release", async () => {
    const fixture = releaseFixture();
    const result = await validate(fixture);
    expect(result).toMatchObject({
      kind: "upstream",
      repository: REPOSITORY,
      repositoryID: REPOSITORY_ID,
      releaseID: 100,
      tag: fixture.currentTag,
      commit: fixture.currentCommit,
      workflowRunID: fixture.currentWorkflowID,
      workflowRunAttempt: 1,
    });
  });

  test("accepts GitHub's stable released action through the same provenance", async () => {
    const fixture = releaseFixture();
    fixture.payload.action = "released";
    await expect(validate(fixture)).resolves.toMatchObject({
      releaseID: 100,
      tag: fixture.currentTag,
      commit: fixture.currentCommit,
    });
  });

  test.each(["created", "edited", "prereleased", "unpublished", "deleted"])(
    "rejects premature or wrong action %s before outbound calls",
    async (action) => {
      const fixture = releaseFixture();
      fixture.payload.action = action;
      let calls = 0;
      const failing: GitHubClient = {
        get: async () => {
          calls++;
          throw new Error("network must not run");
        },
        bytes: async () => {
          calls++;
          throw new Error("network must not run");
        },
      };
      await expect(
        validateRelease(
          fixture.payload,
          receivedAt,
          failing,
          fixture.registry.client,
          signal,
          { now },
        ),
      ).rejects.toThrow("action is not a stable release publication");
      expect(calls).toBe(0);
    },
  );

  test.each([
    ["canonical assets", (f:ReleaseFixture)=>f.canonical.assets.pop()],
    ["latest", (f:ReleaseFixture)=>f.values.get(`/repos/${REPOSITORY}/releases/latest`).id++],
    ["tag object", (f:ReleaseFixture)=>f.values.get(`/repos/${REPOSITORY}/git/ref/tags/${f.currentTag}`).object.sha="f".repeat(40)],
    ["main", (f:ReleaseFixture)=>f.values.get(`/repos/${REPOSITORY}/commits/main`).sha="e".repeat(40)],
    ["registry", (f:ReleaseFixture)=>{f.registry.index.manifests=[];f.registry.refreshIndex();}],
  ])("final recheck rejects %s race", async (_name, mutate) => {
    const fixture=releaseFixture(), verified=await validate(fixture); mutate(fixture);
    await expect(revalidateRelease(verified,fixture.github,fixture.registry.client,signal)).rejects.toThrow();
  });

  test.each([
    [
      "payload repository",
      (f: ReleaseFixture) => {
        f.payload.repository.id++;
      },
    ],
    [
      "payload owner",
      (f: ReleaseFixture) => {
        f.payload.repository.owner.id++;
      },
    ],
    [
      "sender",
      (f: ReleaseFixture) => {
        f.payload.sender.id++;
      },
    ],
    [
      "canonical repository",
      (f: ReleaseFixture) => {
        f.values.get(`/repos/${REPOSITORY}`).id++;
      },
    ],
    [
      "release author",
      (f: ReleaseFixture) => {
        f.canonical.author.id++;
      },
    ],
    ["payload release author", (f: ReleaseFixture) => { f.payload.release.author.id++; }],
    ["by-tag release author", (f: ReleaseFixture) => {
      f.values.get(`/repos/${REPOSITORY}/releases/tags/${f.currentTag}`).author.id++;
    }],
  ])("rejects spoofed %s identity", async (_name, mutate) => {
    const fixture = releaseFixture();
    mutate(fixture);
    await expect(validate(fixture)).rejects.toBeInstanceOf(RejectedDelivery);
  });

  test("rejects unintended tag grammar before API lookups", async () => {
    const fixture = releaseFixture();
    fixture.payload.release.tag_name = "v7.2.132";
    await expect(validate(fixture)).rejects.toThrow("tag grammar");
  });

  test("rejects a lightweight tag", async () => {
    const fixture = releaseFixture();
    fixture.values.get(
      `/repos/${REPOSITORY}/git/ref/tags/${fixture.currentTag}`,
    ).object.type = "commit";
    await expect(validate(fixture)).rejects.toThrow("tag ref");
  });

  test("rejects a moved main", async () => {
    const fixture = releaseFixture();
    fixture.values.get(`/repos/${REPOSITORY}/commits/main`).sha = "f".repeat(
      40,
    );
    await expect(validate(fixture)).rejects.toThrow("tag, commit, or main");
  });

  test("rejects a superseded release", async () => {
    const fixture = releaseFixture();
    fixture.values.get(`/repos/${REPOSITORY}/releases/latest`).id++;
    await expect(validate(fixture)).rejects.toThrow("latest");
  });

  test("rejects payload URL drift and latest tag drift", async () => {
    const url = releaseFixture();
    url.payload.release.html_url += "/spoof";
    await expect(validate(url)).rejects.toThrow("payload release URL");
    const latest = releaseFixture();
    latest.values.get(`/repos/${REPOSITORY}/releases/latest`).tag_name += "-moved";
    await expect(validate(latest)).rejects.toThrow("latest");
  });

  test.each([
    ["draft", (fixture: ReleaseFixture) => { fixture.payload.release.draft = true; }],
    ["prerelease", (fixture: ReleaseFixture) => { fixture.payload.release.prerelease = true; }],
  ])("rejects %s stable-action payload status", async (_name, mutate) => {
    const fixture = releaseFixture();
    fixture.payload.action = "released";
    mutate(fixture);
    await expect(validate(fixture)).rejects.toThrow("status");
  });

  test("rejects a stale original delivery", async () => {
    const fixture = releaseFixture();
    await expect(
      validateRelease(
        fixture.payload,
        "2026-08-16T05:35:00Z",
        fixture.github,
        fixture.registry.client,
        signal,
        { now },
      ),
    ).rejects.toThrow("stale or future");
  });

  test("stale signed payload fails before a network failure", async () => {
    const fixture = releaseFixture();
    let calls = 0;
    const failing: GitHubClient = {
      get: async () => { calls++; throw new Error("malicious network diagnostic"); },
      bytes: async () => { calls++; throw new Error("malicious network diagnostic"); },
    };
    await expect(validateRelease(
      fixture.payload,
      "2026-08-16T05:35:00Z",
      failing,
      fixture.registry.client,
      signal,
      { now },
    )).rejects.toBeInstanceOf(RejectedDelivery);
    expect(calls).toBe(0);
  });

  test.each([
    "2026-08-15T05:32:46+00:00",
    "2026-02-30T05:32:46Z",
  ])("malformed or offset payload timestamp %s makes zero outbound calls", async (timestamp) => {
    const fixture = releaseFixture();
    fixture.payload.release.published_at = timestamp;
    let calls = 0;
    const failing: GitHubClient = {
      get: async () => { calls++; throw new Error("network must not run"); },
      bytes: async () => { calls++; throw new Error("network must not run"); },
    };
    await expect(validateRelease(
      fixture.payload,
      receivedAt,
      failing,
      fixture.registry.client,
      signal,
      { now },
    )).rejects.toBeInstanceOf(RejectedDelivery);
    expect(calls).toBe(0);
  });

  test.each([
    "2026-08-15T05:35:00+00:00",
    "2026-08-15T05:35:00.1Z",
    "2026-02-30T05:35:00Z",
  ])("rejects non-canonical receivedAt %s before outbound calls", async (timestamp) => {
    const fixture = releaseFixture();
    let calls = 0;
    const failing: GitHubClient = {
      get: async () => { calls++; throw new Error("network must not run"); },
      bytes: async () => { calls++; throw new Error("network must not run"); },
    };
    await expect(validateRelease(
      fixture.payload,
      timestamp,
      failing,
      fixture.registry.client,
      signal,
      { now },
    )).rejects.toBeInstanceOf(RejectedDelivery);
    expect(calls).toBe(0);
  });

  test("accepts a canonical millisecond receivedAt", async () => {
    const fixture = releaseFixture();
    await expect(validateRelease(
      fixture.payload,
      "2026-08-15T05:35:00.000Z",
      fixture.github,
      fixture.registry.client,
      signal,
      { now },
    )).resolves.toBeDefined();
  });

  test("rejects a stale retry even when receivedAt was fresh", async () => {
    const fixture = releaseFixture();
    await expect(
      validate(fixture, { now: () => Date.parse("2026-08-16T05:35:00Z") }),
    ).rejects.toThrow("stale or future");
  });

  test("rejects a future published timestamp", async () => {
    const fixture = releaseFixture();
    fixture.payload.release.published_at = "2026-08-15T06:00:00Z";
    fixture.canonical.published_at = fixture.payload.release.published_at;
    (
      fixture.values.get(
        `/repos/${REPOSITORY}/releases/tags/${fixture.currentTag}`,
      ) as any
    ).published_at = fixture.payload.release.published_at;
    await expect(validate(fixture)).rejects.toThrow("stale or future");
  });

  test("keeps a fresh release without its receipt retryable", async () => {
    const fixture = releaseFixture();
    fixture.canonical.assets = fixture.canonical.assets.filter(
      (asset: any) => asset !== fixture.receiptAsset,
    );
    await expect(validate(fixture)).rejects.toBeInstanceOf(RetryableNotReady);
  });

  test("historical previous-release missing assets are permanent", async () => {
    const fixture = releaseFixture("hotfix");
    fixture.baseRelease!.assets = fixture.baseRelease!.assets.filter(
      (asset: any) => asset.name !== "upstream-sync-receipt.json",
    );
    await expect(validate(fixture)).rejects.toBeInstanceOf(RejectedDelivery);
  });

  test("rejects both receipt kinds", async () => {
    const fixture = releaseFixture();
    const extra = makeAsset(
      14,
      "hotfix-release-receipt.json",
      bytes("{}"),
      fixture.assetBytes,
    );
    fixture.canonical.assets.push(extra);
    await expect(validate(fixture)).rejects.toThrow("exactly one receipt");
  });

  test("rejects extra receipt fields", async () => {
    const fixture = releaseFixture();
    fixture.receipt.untrusted = true;
    fixture.refreshReceipt();
    await expect(validate(fixture)).rejects.toThrow("fields differ");
  });

  test("rejects receipt bytes that do not match the asset digest", async () => {
    const fixture = releaseFixture();
    fixture.assetBytes.set(fixture.receiptAsset.url, bytes("{}"));
    await expect(validate(fixture)).rejects.toThrow("asset bytes digest");
  });

  test("rejects an unknown release asset", async () => {
    const fixture = releaseFixture();
    fixture.canonical.assets.push(
      makeAsset(15, "notes.txt", bytes("notes"), fixture.assetBytes),
    );
    await expect(validate(fixture)).rejects.toThrow("asset set");
  });

  test("rejects duplicate asset names", async () => {
    const fixture = releaseFixture();
    const duplicate = makeAsset(
      15,
      fixture.archiveAsset.name,
      bytes("other"),
      fixture.assetBytes,
    );
    fixture.canonical.assets.push(duplicate);
    await expect(validate(fixture)).rejects.toThrow("duplicated");
  });

  test("rejects checksum traversal and checksum mismatch", async () => {
    const traversal = releaseFixture();
    traversal.refreshChecksum(`${"a".repeat(64)}  ../archive.tar.gz\n`);
    await expect(validate(traversal)).rejects.toThrow("checksums file");

    const mismatch = releaseFixture();
    mismatch.refreshChecksum(
      `${"a".repeat(64)}  ${mismatch.archiveAsset.name}\n`,
    );
    await expect(validate(mismatch)).rejects.toThrow("checksum coverage");
  });

  test("rejects receipt state and image identity drift", async () => {
    const stateDrift = releaseFixture();
    stateDrift.receipt.plan_fingerprint = "f".repeat(40);
    stateDrift.refreshReceipt();
    await expect(validate(stateDrift)).rejects.toThrow("receipt core");

    const imageDrift = releaseFixture();
    imageDrift.receipt.image = `${IMAGE}:latest`;
    imageDrift.refreshReceipt();
    await expect(validate(imageDrift)).rejects.toThrow("receipt core");
  });

  test.each([
    [
      "path",
      (run: any) => {
        run.path = ".github/workflows/other.yml";
      },
      RejectedDelivery,
    ],
    [
      "actor",
      (run: any) => {
        run.actor.id++;
      },
      RejectedDelivery,
    ],
    [
      "event",
      (run: any) => {
        run.event = "push";
      },
      RejectedDelivery,
    ],
    [
      "failure",
      (run: any) => {
        run.conclusion = "failure";
      },
      RejectedDelivery,
    ],
    [
      "in progress",
      (run: any) => {
        run.status = "in_progress";
        run.conclusion = null;
      },
      RetryableNotReady,
    ],
  ])("rejects or retries workflow %s", async (_name, mutate, errorClass) => {
    const fixture = releaseFixture();
    mutate(
      fixture.values.get(
        `/repos/${REPOSITORY}/actions/runs/${fixture.currentWorkflowID}`,
      ),
    );
    await expect(validate(fixture)).rejects.toBeInstanceOf(errorClass);
  });

  test("leaves network failures retryable instead of converting them to permanent rejection", async () => {
    const fixture = releaseFixture();
    const github: GitHubClient = {
      ...fixture.github,
      get: async () => {
        throw new Error("offline");
      },
    };
    await expect(
      validateRelease(
        fixture.payload,
        receivedAt,
        github,
        fixture.registry.client,
        signal,
        { now },
      ),
    ).rejects.toThrow("offline");
  });
});

describe("hotfix release provenance", () => {
  test("accepts an exact hotfix anchored to an accepted upstream release", async () => {
    const fixture = releaseFixture("hotfix");
    const result = await validate(fixture);
    expect(result).toMatchObject({
      kind: "hotfix",
      tag: fixture.currentTag,
      commit: fixture.currentCommit,
      workflowRunID: fixture.currentWorkflowID,
    });
  });

  test("accepts consecutive schema-v2 hotfix chains through legacy suffix .1", async () => {
    const second = advanceHotfixFixture(releaseFixture("hotfix"));
    await expect(validate(second)).resolves.toMatchObject({
      kind: "hotfix",
      tag: "v7.2.132-unstableneutron.2",
    });
    expect(second.registry.calls.filter((call) => call === "latest")).toHaveLength(1);

    const third = advanceHotfixFixture(second);
    await expect(validate(third)).resolves.toMatchObject({
      kind: "hotfix",
      tag: "v7.2.132-unstableneutron.3",
    });
    expect(third.registry.calls.filter((call) => call === "latest")).toHaveLength(2);
  });

  test.each([
    ["receipt digest", (link: any) => (link.receipt.digest = `sha256:${"f".repeat(64)}`)],
    ["receipt asset", (link: any) => (link.receipt.asset_id = "99999")],
    ["artifact digest", (link: any) => (link.artifact.digest = `sha256:${"f".repeat(64)}`)],
    ["artifact ID", (link: any) => (link.artifact.id = "99999")],
    ["workflow run", (link: any) => (link.workflow.run_id = "99999")],
    ["workflow head", (link: any) => (link.workflow.head_sha = "f".repeat(40))],
  ])("rejects recorded parent %s drift", async (_name, mutate) => {
    const fixture = advanceHotfixFixture(releaseFixture("hotfix"));
    mutate(fixture.receipt.previous_release);
    fixture.refreshReceipt();
    await expect(validate(fixture)).rejects.toThrow();
  });

  test.each([
    ["tag", (root: any) => (root.tag = "v7.2.132-unstableneutron.9")],
    ["commit", (root: any) => (root.commit = "f".repeat(40))],
    ["receipt", (root: any) => (root.receipt.digest = `sha256:${"f".repeat(64)}`)],
    ["artifact", (root: any) => (root.artifact.id = "99999")],
  ])("rejects recorded accepted-root %s drift", async (_name, mutate) => {
    const fixture = advanceHotfixFixture(releaseFixture("hotfix"));
    mutate(fixture.receipt.accepted_upstream_root);
    fixture.refreshReceipt();
    await expect(validate(fixture)).rejects.toThrow();
  });

  test("rejects a chained suffix gap and a repeated chain commit", async () => {
    const gap = advanceHotfixFixture(releaseFixture("hotfix"));
    gap.receipt.previous_release.tag = "v7.2.132-unstableneutron.0";
    gap.refreshReceipt();
    await expect(validate(gap)).rejects.toThrow("tag relationship");

    const cycle = advanceHotfixFixture(releaseFixture("hotfix"));
    cycle.receipt.previous_release.commit = cycle.currentCommit;
    cycle.values.set(
      `/repos/${REPOSITORY}/compare/${cycle.currentCommit}...${cycle.currentCommit}`,
      { status: "ahead" },
    );
    cycle.refreshReceipt();
    await expect(validate(cycle)).rejects.toThrow("cycle");
  });

  test("rejects a hotfix chain beyond the recursion bound", async () => {
    let fixture = releaseFixture("hotfix");
    for (let suffix = 2; suffix <= 33; suffix++) {
      fixture = advanceHotfixFixture(fixture);
    }
    await expect(validate(fixture)).rejects.toThrow("bound");
  });

  test.each([
    ["draft", (release: any) => (release.draft = true)],
    ["prerelease", (release: any) => (release.prerelease = true)],
  ])("rejects a %s historical parent release", async (_name, mutate) => {
    const fixture = advanceHotfixFixture(releaseFixture("hotfix"));
    mutate(fixture.values.get(`/repos/${REPOSITORY}/releases/101`));
    mutate(
      fixture.values.get(
        `/repos/${REPOSITORY}/releases/tags/v7.2.132-unstableneutron.1`,
      ),
    );
    await expect(validate(fixture)).rejects.toThrow("historical release identity");
  });

  test("rejects missing or tampered historical parent release evidence", async () => {
    const missing = advanceHotfixFixture(releaseFixture("hotfix"));
    for (const release of [
      missing.values.get(`/repos/${REPOSITORY}/releases/101`),
      missing.values.get(
        `/repos/${REPOSITORY}/releases/tags/v7.2.132-unstableneutron.1`,
      ),
    ]) {
      release.assets = release.assets.filter(
        (asset: any) => asset.name !== "hotfix-release-receipt.json",
      );
    }
    await expect(validate(missing)).rejects.toThrow("historical release assets");

    const checksum = advanceHotfixFixture(releaseFixture("hotfix"));
    const historical = checksum.values.get(`/repos/${REPOSITORY}/releases/101`);
    const checksumAsset = historical.assets.find(
      (asset: any) => asset.name === "checksums.txt",
    );
    checksum.assetBytes.set(checksumAsset.url, bytes("tampered\n"));
    await expect(validate(checksum)).rejects.toThrow("asset bytes digest");

    const artifact = advanceHotfixFixture(releaseFixture("hotfix"));
    const listing = artifact.values.get(
      `/repos/${REPOSITORY}/actions/runs/900/artifacts?per_page=100`,
    );
    artifact.assetBytes.set(listing.artifacts[0].archive_download_url, bytes("tampered"));
    await expect(validate(artifact)).rejects.toThrow();
  });

  test("rejects historical ancestry and schema-v1 replay after suffix .1", async () => {
    const ancestry = advanceHotfixFixture(releaseFixture("hotfix"));
    ancestry.values.set(
      `/repos/${REPOSITORY}/compare/${ancestry.baseCommit}...${ancestry.receipt.previous_release.commit}`,
      { status: "identical" },
    );
    await expect(validate(ancestry)).rejects.toThrow("historical ancestry");

    const replay = advanceHotfixFixture(releaseFixture("hotfix"));
    replay.receipt.hotfix_schema_version = 1;
    replay.receipt.previous_release = {
      tag: replay.baseTag,
      commit: replay.baseCommit,
    };
    delete replay.receipt.accepted_upstream_root;
    replay.refreshReceipt();
    await expect(validate(replay)).rejects.toThrow("schema-v1 hotfix");
  });

  test("rejects schema-v2 receipts at suffix .1", async () => {
    const fixture = releaseFixture("hotfix");
    fixture.receipt.hotfix_schema_version = 2;
    fixture.receipt.previous_release = fixtureReleaseLink(
      fixture,
      "upstream",
      fixture.baseTag!,
      fixture.baseCommit!,
      fixture.baseReceiptAsset!,
      800,
    );
    fixture.receipt.accepted_upstream_root = structuredClone(
      fixture.receipt.previous_release,
    );
    fixture.refreshReceipt();
    await expect(validate(fixture)).rejects.toThrow(
      "schema-v2 hotfix requires suffix .2 or later",
    );
  });

  test("rejects cross-verifier root run-state, receipt-size, and schema-type drift", async () => {
    const runStateDrift = releaseFixture("hotfix");
    replaceUpstreamRunState(runStateDrift, bytes("{}"));
    await expect(validate(runStateDrift)).rejects.toThrow("run state");

    const rootFingerprintDrift = releaseFixture("hotfix");
    const rootState = JSON.parse(
      new TextDecoder().decode(
        runState(
          rootFingerprintDrift.baseReceipt!,
          rootFingerprintDrift.baseCommit!,
          rootFingerprintDrift.baseTag!,
        ),
      ),
    );
    rootState.final_plan.plan_fingerprint = "f".repeat(40);
    replaceUpstreamRunState(
      rootFingerprintDrift,
      bytes(JSON.stringify(rootState)),
    );
    await expect(validate(rootFingerprintDrift)).rejects.toThrow("run state");

    const sizeDrift = releaseFixture("hotfix");
    for (const release of [
      sizeDrift.values.get(`/repos/${REPOSITORY}/releases/101`),
      sizeDrift.values.get(
        `/repos/${REPOSITORY}/releases/tags/${sizeDrift.currentTag}`,
      ),
    ]) {
      release.assets.find(
        (asset: any) => asset.name === "hotfix-release-receipt.json",
      ).size += 1;
    }
    await expect(validate(sizeDrift)).rejects.toThrow("asset bytes digest");

    const schemaTypeDrift = releaseFixture("hotfix");
    schemaTypeDrift.receipt.hotfix_schema_version = "1";
    schemaTypeDrift.refreshReceipt();
    await expect(validate(schemaTypeDrift)).rejects.toThrow("schema");
  });

  test.each([
    ["accepted root", () => releaseFixture("hotfix"), "v7.2.132-unstableneutron.0"],
    ["immediate hotfix parent", () => advanceHotfixFixture(releaseFixture("hotfix")), "v7.2.132-unstableneutron.1"],
  ])("rejects canonical-vs-by-tag asset drift for the %s", async (_name, makeFixture, historicalTag) => {
    const fixture = makeFixture();
    const byTag = fixture.values.get(
      `/repos/${REPOSITORY}/releases/tags/${historicalTag}`,
    );
    byTag.assets[0].size += 1;
    await expect(validate(fixture)).rejects.toThrow("release identity differs");
  });

  test("accepts semantically exact root run state independent of JSON object order", async () => {
    const fixture = releaseFixture("hotfix");
    const reordered = JSON.parse(
      new TextDecoder().decode(
        runState(fixture.baseReceipt!, fixture.baseCommit!, fixture.baseTag!),
      ),
    );
    reordered.release = {
      architecture_images: reordered.release.architecture_images,
      platforms: reordered.release.platforms,
      image_digest: reordered.release.image_digest,
      image: reordered.release.image,
      assets: reordered.release.assets,
      url: reordered.release.url,
    };
    replaceUpstreamRunState(fixture, bytes(JSON.stringify(reordered)));
    await expect(validate(fixture)).resolves.toMatchObject({ kind: "hotfix" });
  });

  test.each([
    [
      "release",
      (fixture: ReleaseFixture) =>
        `/repos/${REPOSITORY}/releases/tags/v7.2.132-unstableneutron.1`,
      false,
    ],
    [
      "state",
      (fixture: ReleaseFixture) =>
        `/repos/${REPOSITORY}/contents/.ccs-fork-upstream.env?ref=${fixture.receipt.previous_release.commit}`,
      false,
    ],
    ["workflow run", () => `/repos/${REPOSITORY}/actions/runs/900`, false],
    [
      "artifact listing",
      () => `/repos/${REPOSITORY}/actions/runs/900/artifacts?per_page=100`,
      false,
    ],
    [
      "asset download",
      (fixture: ReleaseFixture) => fixture.receipt.previous_release.receipt.digest,
      true,
    ],
  ])(
    "permanently rejects historical %s evidence lost with 404 or 410",
    async (_name, target, download) => {
      for (const status of [404, 410]) {
        const fixture = advanceHotfixFixture(releaseFixture("hotfix"));
        const targetValue = target(fixture);
        const original = fixture.github;
        const github: GitHubClient = {
          get: async (path, requestSignal) => {
            if (!download && path === targetValue) {
              throw new GitHubHTTPError(status, `GitHub API returned ${status}`);
            }
            return original.get(path, requestSignal);
          },
          bytes: async (url, requestSignal) => {
            const parentReceipt = fixture.values
              .get(`/repos/${REPOSITORY}/releases/101`)
              .assets.find(
                (asset: any) => asset.name === "hotfix-release-receipt.json",
              );
            if (download && url === parentReceipt.url) {
              throw new GitHubHTTPError(status, `GitHub asset returned ${status}`);
            }
            return original.bytes(url, requestSignal);
          },
        };
        await expect(
          validateRelease(
            fixture.payload,
            receivedAt,
            github,
            fixture.registry.client,
            signal,
            { now },
          ),
        ).rejects.toBeInstanceOf(RejectedDelivery);
      }
    },
  );

  test.each([[429], [500]])(
    "keeps historical transient HTTP %s failures retryable",
    async (status) => {
      const fixture = advanceHotfixFixture(releaseFixture("hotfix"));
      const original = fixture.github;
      const github: GitHubClient = {
        get: async (path, requestSignal) => {
          if (
            path ===
            `/repos/${REPOSITORY}/releases/tags/v7.2.132-unstableneutron.1`
          ) {
            throw new GitHubHTTPError(status, `GitHub API returned ${status}`);
          }
          return original.get(path, requestSignal);
        },
        bytes: (url, requestSignal) => original.bytes(url, requestSignal),
      };
      await expect(
        validateRelease(
          fixture.payload,
          receivedAt,
          github,
          fixture.registry.client,
          signal,
          { now },
        ),
      ).rejects.toBeInstanceOf(GitHubHTTPError);
    },
  );

  test.each([
    ["schema-v1", () => releaseFixture("hotfix")],
    ["schema-v2", () => advanceHotfixFixture(releaseFixture("hotfix"))],
  ])(
    "classifies %s immediate-parent comparison HTTP failures",
    async (_name, makeFixture) => {
      for (const status of [404, 410, 429, 500]) {
        const fixture = makeFixture();
        const comparison = `/repos/${REPOSITORY}/compare/${fixture.receipt.previous_release.commit}...${fixture.currentCommit}`;
        const original = fixture.github;
        const github: GitHubClient = {
          get: async (path, requestSignal) => {
            if (path === comparison) {
              throw new GitHubHTTPError(status, `GitHub API returned ${status}`);
            }
            return original.get(path, requestSignal);
          },
          bytes: (url, requestSignal) => original.bytes(url, requestSignal),
        };
        const validation = validateRelease(
          fixture.payload,
          receivedAt,
          github,
          fixture.registry.client,
          signal,
          { now },
        );
        if (status === 404 || status === 410) {
          await expect(validation).rejects.toBeInstanceOf(RejectedDelivery);
        } else {
          await expect(validation).rejects.toBeInstanceOf(GitHubHTTPError);
        }
      }
    },
  );

  test.each([
    ["original_tag", "v7.2.131"],
    ["plus_tag", "v7.2.127-2"],
    ["pre_sync_head", "f".repeat(40)],
    ["base_fork_commit", "f".repeat(40)],
    ["original_repository", "wrong/original"],
    ["plus_repository", "wrong/plus"],
    ["models_repository", "wrong/models"],
    ["original_head", "f".repeat(40)],
    ["plus_tag_head", "f".repeat(40)],
    ["plus_head", "f".repeat(40)],
    ["models_commit", "f".repeat(40)],
    ["plus_head_included", "true"],
    ["plus_head_already_represented", "false"],
    ["plus_head_delta_paths", "internal/example.go"],
    ["unsafe_plus_head_delta_paths", "README.md"],
    ["block_reason", "unexpected"],
    ["fork_tag_prefix", "v7.2.131-unstableneutron"],
    ["latest_fork_tag", "v7.2.132-unstableneutron.9"],
    ["latest_fork_models_commit", "f".repeat(40)],
    ["latest_fork_suffix", "9"],
    ["next_fork_tag", "v7.2.132-unstableneutron.9"],
    ["expected_fork_tag", "v7.2.132-unstableneutron.9"],
    ["safe_sync_id", "wrong"],
    ["plan_fingerprint", "f".repeat(40)],
    ["candidate_branch", "upstream-sync/wrong"],
    ["snapshot_namespace", "refs/upstream-sync/wrong"],
    ["original_snapshot_ref", "refs/upstream-sync/wrong/original"],
    ["plus_tag_snapshot_ref", "refs/upstream-sync/wrong/plus-tag"],
    ["plus_head_snapshot_ref", "refs/upstream-sync/wrong/plus-head"],
    ["models_snapshot_ref", "refs/upstream-sync/wrong/models"],
    ["target_drift_summary", "unexpected"],
    ["has_changes", "true"],
    ["target_drift", "true"],
    ["blocked", "true"],
  ])("rejects a wrong final planner %s", async (key, wrongValue) => {
    const fixture = releaseFixture("hotfix");
    const plan = new TextDecoder()
      .decode(finalPlan(fixture.currentTag, fixture.currentCommit))
      .replace(new RegExp(`^${key}=.*$`, "m"), `${key}=${wrongValue}`);
    replaceHotfixFinalPlan(fixture, bytes(plan));
    await expect(validate(fixture)).rejects.toThrow("final plan identity");
  });

  test.each([
    ["missing", (plan: string) => plan.replace(/^original_tag=.*\n/m, "")],
    ["extra", (plan: string) => `${plan}unexpected_field=value\n`],
  ])("rejects a final plan with a %s field", async (_name, mutate) => {
    const fixture = releaseFixture("hotfix");
    const plan = new TextDecoder().decode(
      finalPlan(fixture.currentTag, fixture.currentCommit),
    );
    replaceHotfixFinalPlan(fixture, bytes(mutate(plan)));
    await expect(validate(fixture)).rejects.toThrow("final plan fields differ");
  });

  test("rejects a non-next hotfix suffix", async () => {
    const fixture = releaseFixture("hotfix");
    fixture.receipt.previous_release.tag = "v7.2.132-unstableneutron.9";
    fixture.refreshReceipt();
    await expect(validate(fixture)).rejects.toThrow("tag relationship");
  });

  test("rejects a hotfix not strictly ahead of its base", async () => {
    const fixture = releaseFixture("hotfix");
    fixture.values.get(
      `/repos/${REPOSITORY}/compare/${fixture.baseCommit}...${fixture.currentCommit}`,
    ).status = "identical";
    await expect(validate(fixture)).rejects.toThrow("strictly ahead");
  });

  test("requires the accepted root commit to be strictly behind a schema-v1 suffix .1", async () => {
    const fixture = releaseFixture("hotfix");
    fixture.values.get(
      `/repos/${REPOSITORY}/compare/${fixture.baseCommit}...${fixture.currentCommit}`,
    ).status = "identical";
    await expect(validate(fixture)).rejects.toThrow("strictly ahead");
    expect(fixture.registry.calls.filter((call) => call === "latest")).toHaveLength(0);
  });

  test("rejects changed upstream state between base and hotfix", async () => {
    const fixture = releaseFixture("hotfix");
    const changed = stateFile(fixture.baseTag!).map((value, index) =>
      index === 0 ? value : value,
    );
    const text = new TextDecoder()
      .decode(changed)
      .replace("PLUS_HEAD_INCLUDED=false", "PLUS_HEAD_INCLUDED=true");
    fixture.values.set(
      `/repos/${REPOSITORY}/contents/.ccs-fork-upstream.env?ref=${fixture.baseCommit}`,
      encodedContent(bytes(text)),
    );
    await expect(validate(fixture)).rejects.toThrow("changed upstream state");
  });

  test("rejects a malformed previous accepted receipt", async () => {
    const fixture = releaseFixture("hotfix");
    fixture.baseReceipt!.plan_fingerprint = "f".repeat(40);
    fixture.refreshBaseReceipt!();
    await expect(validate(fixture)).rejects.toThrow("receipt core");
  });

  test("rejects a spoofed previous release author", async () => {
    const fixture = releaseFixture("hotfix");
    fixture.baseRelease!.author.id++;
    await expect(validate(fixture)).rejects.toThrow("previous release author");
  });

  test("rejects upstream state digest and asset digest map drift", async () => {
    const stateDrift = releaseFixture("hotfix");
    stateDrift.receipt.upstream_state.sha256 = "f".repeat(64);
    stateDrift.refreshReceipt();
    await expect(validate(stateDrift)).rejects.toThrow(
      "upstream state receipt",
    );

    const assetDrift = releaseFixture("hotfix");
    assetDrift.receipt.release_asset_digests["checksums.txt"] =
      `sha256:${"f".repeat(64)}`;
    assetDrift.refreshReceipt();
    await expect(validate(assetDrift)).rejects.toThrow("asset digests");
  });

  test("rejects hotfix workflow receipt attempt mismatch", async () => {
    const fixture = releaseFixture("hotfix");
    fixture.receipt.release_workflow.run_attempt = "2";
    fixture.refreshReceipt();
    await expect(validate(fixture)).rejects.toThrow("workflow attempt");
  });

  test("rejects historical index and architecture tag drift without requesting latest", async () => {
    const index = releaseFixture("hotfix");
    index.baseReceipt!.image_digest = `sha256:${"f".repeat(64)}`;
    index.refreshBaseReceipt!();
    await expect(validate(index)).rejects.toThrow();
    expect(index.registry.calls.filter((call) => call === "latest")).toHaveLength(0);

    const architecture = releaseFixture("hotfix");
    architecture.registry.manifests.get(`${architecture.baseTag}-amd64`)!.digest = `sha256:${"f".repeat(64)}`;
    await expect(validate(architecture)).rejects.toThrow("digest");
    expect(architecture.registry.calls.filter((call) => call === "latest")).toHaveLength(0);
  });
});

describe("canonical registry verification", () => {
  test("accepts exact tag, latest, platform, architecture tag, and receipt parity", async () => {
    const fixture = registryFixture("v1.2.3-unstableneutron.0");
    const result = await verifyRegistry(
      fixture.client,
      "v1.2.3-unstableneutron.0",
      fixture.manifests.get("latest")!.digest,
      fixture.architectureImages,
      signal,
    );
    expect(result.amd64).toBe(fixture.architectureImages["linux/amd64"].digest);
  });

  test("rejects latest digest drift", async () => {
    const fixture = registryFixture("v1.2.3-unstableneutron.0");
    const latest = structuredClone(fixture.manifests.get("latest")!);
    latest.bytes = bytes(
      JSON.stringify({
        schemaVersion: 2,
        mediaType: indexMediaType,
        manifests: [],
      }),
    );
    latest.digest = sha256(latest.bytes);
    fixture.manifests.set("latest", latest);
    await expect(
      verifyRegistry(
        fixture.client,
        "v1.2.3-unstableneutron.0",
        fixture.manifests.get("v1.2.3-unstableneutron.0")!.digest,
        fixture.architectureImages,
        signal,
      ),
    ).rejects.toThrow("canonical image");
  });

  test.each([
    ["missing", (manifests: any[]) => manifests.splice(2, 1)],
    [
      "duplicate",
      (manifests: any[]) => manifests.push(structuredClone(manifests[0])),
    ],
    [
      "extra",
      (manifests: any[]) =>
        manifests.push({
          digest: `sha256:${"f".repeat(64)}`,
          mediaType: manifestMediaType,
          platform: { os: "linux", architecture: "s390x" },
        }),
    ],
  ])("rejects a %s platform", async (_name, mutate) => {
    const fixture = registryFixture("v1.2.3-unstableneutron.0");
    mutate(fixture.index.manifests as any[]);
    const expected = fixture.refreshIndex();
    await expect(
      verifyRegistry(
        fixture.client,
        "v1.2.3-unstableneutron.0",
        expected,
        fixture.architectureImages,
        signal,
      ),
    ).rejects.toThrow("platform");
  });

  test("rejects unknown descriptors without exact attestation identity", async () => {
    const fixture = registryFixture("v1.2.3-unstableneutron.0");
    const attestation = (fixture.index.manifests as any[])[1];
    delete attestation.annotations["vnd.docker.reference.type"];
    const expected = fixture.refreshIndex();
    await expect(
      verifyRegistry(
        fixture.client,
        "v1.2.3-unstableneutron.0",
        expected,
        fixture.architectureImages,
        signal,
      ),
    ).rejects.toThrow("attestation");
  });

  test("rejects required and unknown platform metadata plus duplicate attestation subject", async () => {
    for (const index of [0, 1]) {
      const fixture = registryFixture("v1.2.3-unstableneutron.0");
      (fixture.index.manifests as any[])[index].platform.variant = "v8";
      const expected = fixture.refreshIndex();
      await expect(verifyRegistry(fixture.client, "v1.2.3-unstableneutron.0", expected, fixture.architectureImages, signal)).rejects.toThrow("platform");
    }
    const duplicate = registryFixture("v1.2.3-unstableneutron.0");
    (duplicate.index.manifests as any[])[3].annotations["vnd.docker.reference.digest"] =
      (duplicate.index.manifests as any[])[1].annotations["vnd.docker.reference.digest"];
    const expected = duplicate.refreshIndex();
    await expect(verifyRegistry(duplicate.client, "v1.2.3-unstableneutron.0", expected, duplicate.architectureImages, signal)).rejects.toThrow("attestation subject");
  });

  test("rejects architecture tag and receipt digest drift", async () => {
    const tag = "v1.2.3-unstableneutron.0";
    const architectureDrift = registryFixture(tag);
    architectureDrift.manifests.get(`${tag}-amd64`)!.digest =
      `sha256:${"f".repeat(64)}`;
    await expect(
      verifyRegistry(
        architectureDrift.client,
        tag,
        architectureDrift.manifests.get(tag)!.digest,
        architectureDrift.architectureImages,
        signal,
      ),
    ).rejects.toThrow("digest");

    const receiptDrift = registryFixture(tag);
    receiptDrift.architectureImages["linux/arm64"].digest =
      `sha256:${"f".repeat(64)}`;
    await expect(
      verifyRegistry(
        receiptDrift.client,
        tag,
        receiptDrift.manifests.get(tag)!.digest,
        receiptDrift.architectureImages,
        signal,
      ),
    ).rejects.toThrow("receipt architecture");
  });
});
