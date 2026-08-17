import { createHash } from "node:crypto";
import { readZipBasenames } from "./zip";

export const REPOSITORY = "unstableneutron/CLIProxyAPIPlus";
export const REPOSITORY_ID = 1247056725;
export const OWNER_LOGIN = "unstableneutron";
export const OWNER_ID = 156744497;
export const BOT_LOGIN = "github-actions[bot]";
export const BOT_ID = 41898282;
export const IMAGE = "ghcr.io/unstableneutron/cli-proxy-api-plus";

const RELEASE_TAG = /^v[0-9]+\.[0-9]+\.[0-9]+-unstableneutron\.[0-9]+$/;
const SOURCE_TAG = /^v[0-9A-Za-z][0-9A-Za-z._+-]*$/;
const SHA = /^[0-9a-f]{40}$/;
const SHA256 = /^[0-9a-f]{64}$/;
const DIGEST = /^sha256:[0-9a-f]{64}$/;
const MAXIMUM_RELEASE_AGE_MS = 12 * 60 * 60 * 1000;
const MAXIMUM_CLOCK_SKEW_MS = 5 * 60 * 1000;
const MAXIMUM_STATE_BYTES = 64_000;
const MAXIMUM_METADATA_BYTES = 1_000_000;
const MAXIMUM_ASSET_BYTES = 2_000_000_000;

const UPSTREAM_WORKFLOW = ".github/workflows/upstream-sync-v2.yml";
const HOTFIX_WORKFLOW = ".github/workflows/hotfix-release.yml";

const STATE_KEYS = [
  "SCHEMA_VERSION",
  "SYNC_ID",
  "PLAN_FINGERPRINT",
  "BASE_FORK_COMMIT",
  "ORIGINAL_REPOSITORY",
  "ORIGINAL_TAG",
  "ORIGINAL_COMMIT",
  "PLUS_REPOSITORY",
  "PLUS_TAG",
  "PLUS_TAG_COMMIT",
  "PLUS_HEAD_COMMIT",
  "PLUS_HEAD_INCLUDED",
  "MODELS_REPOSITORY",
  "MODELS_COMMIT",
  "EXPECTED_FORK_TAG",
  "CANDIDATE_BRANCH",
] as const;

const RECEIPT_CORE_KEYS = [
  "schema_version",
  "sync_id",
  "plan_fingerprint",
  "main_commit",
  "tag",
  "tag_commit",
  "release_url",
  "release_assets",
  "image",
  "image_digest",
  "platforms",
  "workflow_run_id",
  "architecture_images",
] as const;

const HOTFIX_RECEIPT_KEYS = [
  ...RECEIPT_CORE_KEYS,
  "receipt_type",
  "hotfix_schema_version",
  "previous_release",
  "upstream_state",
  "release_asset_digests",
  "release_workflow",
] as const;
const HOTFIX_RECEIPT_V2_KEYS = [...HOTFIX_RECEIPT_KEYS, "accepted_upstream_root"] as const;
const RELEASE_LINK_KEYS = ["tag", "commit", "receipt", "workflow", "artifact"] as const;

const INDEX_MEDIA_TYPES = new Set([
  "application/vnd.oci.image.index.v1+json",
  "application/vnd.docker.distribution.manifest.list.v2+json",
]);

const MANIFEST_MEDIA_TYPES = new Set([
  "application/vnd.oci.image.manifest.v1+json",
  "application/vnd.docker.distribution.manifest.v2+json",
]);

type JSONObject = Record<string, unknown>;

export class RejectedDelivery extends Error {}
export class RetryableNotReady extends Error {}
export class GitHubHTTPError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "GitHubHTTPError";
  }
}

export interface GitHubClient {
  get(path: string, signal: AbortSignal): Promise<unknown>;
  bytes(url: string, signal: AbortSignal): Promise<Uint8Array>;
}

export interface RegistryManifest {
  bytes: Uint8Array;
  digest: string;
  mediaType: string;
}

export interface RegistryClient {
  manifest(reference: string, signal: AbortSignal): Promise<RegistryManifest>;
}

export interface VerifiedRelease {
  repository: string;
  repositoryID: number;
  releaseID: number;
  releaseURL: string;
  tag: string;
  publishedAt: string;
  commit: string;
  kind: "upstream" | "hotfix";
  syncID: string;
  planFingerprint: string;
  imageDigest: string;
  architectures: { amd64: string; arm64: string };
  workflowPath: string;
  workflowRunID: number;
  workflowRunAttempt: number;
  /** Opaque inputs retained only for the immediate pre-claim resampling. */
  revalidation: { payload: unknown; receivedAt: string; now: () => number };
}

export interface ValidateReleaseOptions {
  now?: () => number;
}

interface UpstreamState extends Record<(typeof STATE_KEYS)[number], string> {}

interface Asset {
  raw: JSONObject;
  id: number;
  name: string;
  url: string;
  size: number;
  digest: string;
}

interface ReleaseAssets {
  all: Asset[];
  receipts: Asset[];
  checksum: Asset;
  archives: Asset[];
  nonReceipt: Asset[];
}

interface AnnotatedTag {
  tagObjectSHA: string;
  commit: string;
  tagger: JSONObject;
  message: string;
}

interface ReceiptCore {
  workflowRunID: number;
  imageDigest: string;
  architectureImages: JSONObject;
}

const PLAN_KEYS = ["original_tag","plus_tag","pre_sync_head","base_fork_commit","original_repository","plus_repository","models_repository","original_head","plus_tag_head","plus_head","models_commit","plus_head_included","plus_head_already_represented","plus_head_delta_paths","unsafe_plus_head_delta_paths","blocked","block_reason","fork_tag_prefix","latest_fork_tag","latest_fork_models_commit","latest_fork_suffix","next_fork_tag","expected_fork_tag","safe_sync_id","plan_fingerprint","candidate_branch","snapshot_namespace","original_snapshot_ref","plus_tag_snapshot_ref","plus_head_snapshot_ref","models_snapshot_ref","target_drift","target_drift_summary","has_changes"];

function parseFinalPlan(bytes: Uint8Array, state: UpstreamState, tag: string, commit: string): void {
  let text: string;
  try { text = new TextDecoder("utf-8", { fatal: true }).decode(bytes); } catch { throw new RejectedDelivery("final plan encoding differs"); }
  const values: JSONObject = {};
  if (!text.endsWith("\n")) throw new RejectedDelivery("final plan syntax differs");
  for (const line of text.slice(0, -1).split("\n")) {
    const match = /^([a-z][a-z0-9_]*)=(.*)$/.exec(line);
    if (!match || match[1] in values) throw new RejectedDelivery("final plan syntax differs");
    values[match[1]] = match[2];
  }
  exactKeys(values, PLAN_KEYS, "final plan");
  for (const key of ["pre_sync_head","base_fork_commit","original_head","plus_tag_head","plus_head","models_commit","plan_fingerprint"])
    sha(values[key], `final plan ${key}`);
  for (const key of ["plus_head_included","plus_head_already_represented","blocked","target_drift","has_changes"])
    if (values[key] !== "true" && values[key] !== "false") throw new RejectedDelivery("final plan boolean differs");
  if (values.original_tag !== state.ORIGINAL_TAG || values.plus_tag !== state.PLUS_TAG ||
      values.pre_sync_head !== commit || values.base_fork_commit !== commit ||
      values.original_repository !== state.ORIGINAL_REPOSITORY || values.plus_repository !== state.PLUS_REPOSITORY ||
      values.models_repository !== state.MODELS_REPOSITORY || values.original_head !== state.ORIGINAL_COMMIT ||
      values.plus_tag_head !== state.PLUS_TAG_COMMIT || values.plus_head !== state.PLUS_HEAD_COMMIT || values.models_commit !== state.MODELS_COMMIT ||
      values.plus_head_included !== state.PLUS_HEAD_INCLUDED || values.safe_sync_id !== state.SYNC_ID ||
      values.latest_fork_tag !== tag || values.expected_fork_tag !== tag || values.latest_fork_models_commit !== state.MODELS_COMMIT ||
      values.has_changes !== "false" || values.target_drift !== "false" || values.blocked !== "false")
    throw new RejectedDelivery("final plan identity differs");
}

function validateRunState(bytes: Uint8Array, state: UpstreamState, receipt: JSONObject, commit: string, tag: string): void {
  const run = object(parseJSON(bytes, "run state"), "run state");
  exactKeys(run, ["schema_version","state","target","candidate","repair","final_plan","runtime_smoke","vn3_deployed","promotion","release"], "run state");
  const target = object(run.target, "run state target"), candidate = object(run.candidate, "run state candidate"), repair = object(run.repair, "run state repair");
  const final = object(run.final_plan, "run state final plan"), promotion = object(run.promotion, "run state promotion"), release = object(run.release, "run state release");
  exactKeys(target,["base_fork_commit","original","plus","models_commit","sync_id","plan_fingerprint","expected_fork_tag","target_drift","blocked"],"run state target");
  exactKeys(candidate,["branch","sha","acceptable","validation_status"],"run state candidate"); exactKeys(repair,["imported","pr","sha"],"run state repair");
  exactKeys(final,["status","plan_fingerprint","has_changes","target_drift","blocked"],"run state final plan"); exactKeys(promotion,["commit","tag"],"run state promotion");
  exactKeys(release,["url","assets","image","image_digest","platforms","architecture_images"],"run state release");
  const original=object(target.original,"run state original"), plus=object(target.plus,"run state plus"); exactKeys(original,["tag","commit"],"run state original"); exactKeys(plus,["tag","tag_commit","head","head_included"],"run state plus");
  if (run.schema_version!==1 || run.state!=="released" || target.base_fork_commit!==state.BASE_FORK_COMMIT || original.tag!==state.ORIGINAL_TAG || original.commit!==state.ORIGINAL_COMMIT || plus.tag!==state.PLUS_TAG || plus.tag_commit!==state.PLUS_TAG_COMMIT || plus.head!==state.PLUS_HEAD_COMMIT || plus.head_included!==(state.PLUS_HEAD_INCLUDED==="true") || target.models_commit!==state.MODELS_COMMIT || target.sync_id!==state.SYNC_ID || target.plan_fingerprint!==state.PLAN_FINGERPRINT || target.expected_fork_tag!==tag || typeof target.target_drift!=="boolean" || target.blocked!==false || candidate.branch!==state.CANDIDATE_BRANCH || candidate.sha!==commit || candidate.acceptable!==true || candidate.validation_status!=="passed" || promotion.commit!==commit || promotion.tag!==tag || final.status!=="clean-noop" || !SHA.test(string(final.plan_fingerprint,"final fingerprint")) || final.has_changes!==false || final.target_drift!==false || final.blocked!==false || run.runtime_smoke!=="not_run" || run.vn3_deployed!==false || JSON.stringify(release)!==JSON.stringify({url:receipt.release_url,assets:receipt.release_assets,image:receipt.image,image_digest:receipt.image_digest,platforms:receipt.platforms,architecture_images:receipt.architecture_images})) throw new RejectedDelivery("run state identity differs");
  if (repair.imported===false ? (repair.pr!==null || repair.sha!==null) : (repair.imported!==true || !Number.isSafeInteger(repair.pr) || (repair.pr as number) <= 0 || repair.sha!==commit)) throw new RejectedDelivery("run state repair differs");
}

function object(value: unknown, field: string): JSONObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new RejectedDelivery(`${field} is not an object`);
  }
  return value as JSONObject;
}

function string(value: unknown, field: string): string {
  if (typeof value !== "string" || value.length === 0) {
    throw new RejectedDelivery(`${field} is invalid`);
  }
  return value;
}

function integer(value: unknown, field: string): number {
  if (!Number.isSafeInteger(value))
    throw new RejectedDelivery(`${field} is invalid`);
  return value as number;
}

function decimalInteger(value: unknown, field: string): number {
  const text = string(value, field);
  if (!/^[1-9][0-9]*$/.test(text))
    throw new RejectedDelivery(`${field} is invalid`);
  const parsed = Number(text);
  if (!Number.isSafeInteger(parsed))
    throw new RejectedDelivery(`${field} is invalid`);
  return parsed;
}

function exactKeys(
  value: JSONObject,
  expected: readonly string[],
  field: string,
): void {
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  if (
    actual.length !== wanted.length ||
    actual.some((key, index) => key !== wanted[index])
  ) {
    throw new RejectedDelivery(`${field} fields differ`);
  }
}

function sha(value: unknown, field: string): string {
  const result = string(value, field);
  if (!SHA.test(result)) throw new RejectedDelivery(`${field} is invalid`);
  return result;
}

function digest(value: unknown, field: string): string {
  const result = string(value, field);
  if (!DIGEST.test(result)) throw new RejectedDelivery(`${field} is invalid`);
  return result;
}

function digestBytes(bytes: Uint8Array): string {
  return `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
}

function identity(
  value: unknown,
  login: string,
  id: number,
  type: string,
  field: string,
): void {
  const candidate = object(value, field);
  if (
    candidate.login !== login ||
    candidate.id !== id ||
    candidate.type !== type
  ) {
    throw new RejectedDelivery(`${field} identity differs`);
  }
}

function repository(value: unknown, field: string): void {
  const candidate = object(value, field);
  if (candidate.full_name !== REPOSITORY || candidate.id !== REPOSITORY_ID) {
    throw new RejectedDelivery(`${field} identity differs`);
  }
  identity(candidate.owner, OWNER_LOGIN, OWNER_ID, "User", `${field} owner`);
}

function parseJSON(bytes: Uint8Array, field: string): unknown {
  try {
    return JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
  } catch {
    throw new RejectedDelivery(`${field} is malformed`);
  }
}

function compareSortedStrings(
  value: unknown,
  expected: string[],
  field: string,
): void {
  if (
    !Array.isArray(value) ||
    value.some((entry) => typeof entry !== "string")
  ) {
    throw new RejectedDelivery(`${field} differs`);
  }
  const actual = value as string[];
  if (
    actual.length !== expected.length ||
    actual.some((entry, index) => entry !== expected[index])
  ) {
    throw new RejectedDelivery(`${field} differs`);
  }
}

function validateFreshness(
  publishedAt: string,
  receivedAt: string,
  now: number,
): void {
  const published = canonicalUtcTimestamp(publishedAt, false);
  const received = canonicalUtcTimestamp(receivedAt, true);
  const ages = [received - published, now - published];
  if (
    !Number.isFinite(published) ||
    !Number.isFinite(received) ||
    !Number.isFinite(now) ||
    ages.some(
      (age) => age > MAXIMUM_RELEASE_AGE_MS || age < -MAXIMUM_CLOCK_SKEW_MS,
    )
  ) {
    throw new RejectedDelivery("release timestamp is stale or future");
  }
}

function canonicalUtcTimestamp(value: string, allowMilliseconds: boolean): number {
  const grammar = allowMilliseconds
    ? /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{3})?Z$/
    : /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/;
  if (!grammar.test(value)) return Number.NaN;
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed)) return Number.NaN;
  const canonical = new Date(parsed).toISOString();
  const expected = value.includes(".") ? value : value.replace(/Z$/, ".000Z");
  return canonical === expected ? parsed : Number.NaN;
}

function decodeBase64Content(content: JSONObject): Uint8Array {
  if (content.encoding !== "base64")
    throw new RejectedDelivery("state content differs");
  const encoded = string(content.content, "state content").replace(/\n/g, "");
  if (
    encoded.length === 0 ||
    encoded.length % 4 !== 0 ||
    !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(
      encoded,
    )
  ) {
    throw new RejectedDelivery("state content differs");
  }
  const bytes = Buffer.from(encoded, "base64");
  if (bytes.toString("base64") !== encoded || content.size !== bytes.length) {
    throw new RejectedDelivery("state content differs");
  }
  return bytes;
}

export function parseUpstreamState(bytes: Uint8Array): UpstreamState {
  if (bytes.length === 0 || bytes.length > MAXIMUM_STATE_BYTES) {
    throw new RejectedDelivery("state size is invalid");
  }

  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    throw new RejectedDelivery("state encoding is invalid");
  }

  const values: Record<string, string> = {};
  for (const line of text.split("\n")) {
    if (line === "") continue;
    const match = /^([A-Z][A-Z0-9_]*)=(.*)$/.exec(line);
    if (!match || match[1] in values)
      throw new RejectedDelivery("state schema is malformed");
    values[match[1]] = match[2];
  }
  exactKeys(values, STATE_KEYS, "state");

  const state = values as UpstreamState;
  if (
    state.SCHEMA_VERSION !== "2" ||
    state.ORIGINAL_REPOSITORY !== "router-for-me/CLIProxyAPI" ||
    state.PLUS_REPOSITORY !== "kaitranntt/CLIProxyAPIPlus" ||
    state.MODELS_REPOSITORY !== "router-for-me/models"
  ) {
    throw new RejectedDelivery("state identity or schema differs");
  }
  for (const key of [
    "BASE_FORK_COMMIT",
    "ORIGINAL_COMMIT",
    "PLUS_TAG_COMMIT",
    "PLUS_HEAD_COMMIT",
    "MODELS_COMMIT",
  ] as const) {
    if (!SHA.test(state[key]))
      throw new RejectedDelivery("state commit is invalid");
  }
  if (
    !SHA.test(state.PLAN_FINGERPRINT) ||
    !/^(true|false)$/.test(state.PLUS_HEAD_INCLUDED)
  ) {
    throw new RejectedDelivery("state grammar differs");
  }
  if (
    !SOURCE_TAG.test(state.ORIGINAL_TAG) ||
    !SOURCE_TAG.test(state.PLUS_TAG) ||
    !RELEASE_TAG.test(state.EXPECTED_FORK_TAG)
  ) {
    throw new RejectedDelivery("state tag is invalid");
  }

  const syncID = `original-${state.ORIGINAL_TAG}_plus-${state.PLUS_TAG}`;
  const candidateBranch = `upstream-sync/${syncID}-${state.PLAN_FINGERPRINT.slice(0, 12)}`;
  if (state.SYNC_ID !== syncID || state.CANDIDATE_BRANCH !== candidateBranch) {
    throw new RejectedDelivery("state linkage differs");
  }
  return state;
}

async function readStateAt(
  client: GitHubClient,
  commit: string,
  signal: AbortSignal,
): Promise<{ bytes: Uint8Array; state: UpstreamState }> {
  const content = object(
    await client.get(
      `/repos/${REPOSITORY}/contents/.ccs-fork-upstream.env?ref=${commit}`,
      signal,
    ),
    "state content",
  );
  const bytes = decodeBase64Content(content);
  return { bytes, state: parseUpstreamState(bytes) };
}

async function fetchAnnotatedTag(
  client: GitHubClient,
  tag: string,
  signal: AbortSignal,
): Promise<AnnotatedTag> {
  const ref = object(
    await client.get(
      `/repos/${REPOSITORY}/git/ref/tags/${encodeURIComponent(tag)}`,
      signal,
    ),
    "tag ref",
  );
  const refObject = object(ref.object, "tag ref object");
  const tagObjectSHA = sha(refObject.sha, "tag object SHA");
  if (ref.ref !== `refs/tags/${tag}` || refObject.type !== "tag") {
    throw new RejectedDelivery("tag ref differs");
  }

  const annotated = object(
    await client.get(`/repos/${REPOSITORY}/git/tags/${tagObjectSHA}`, signal),
    "annotated tag",
  );
  const target = object(annotated.object, "tag target");
  if (
    annotated.sha !== tagObjectSHA ||
    annotated.tag !== tag ||
    target.type !== "commit"
  ) {
    throw new RejectedDelivery("annotated tag differs");
  }
  return {
    tagObjectSHA,
    commit: sha(target.sha, "tag commit"),
    tagger: object(annotated.tagger, "tagger"),
    message: string(annotated.message, "tag message"),
  };
}

function validateTagger(
  tag: AnnotatedTag,
  kind: "upstream" | "hotfix",
  releaseTag: string,
  baseTag?: string,
): void {
  exactKeys(tag.tagger, ["name", "email", "date"], "tagger");
  const expectedName =
    kind === "upstream"
      ? "cliproxy-upstream-sync[bot]"
      : "cliproxy-hotfix-release[bot]";
  const expectedEmail =
    kind === "upstream"
      ? "cliproxy-upstream-sync@users.noreply.github.com"
      : "cliproxy-hotfix-release@users.noreply.github.com";
  const expectedMessage =
    kind === "upstream"
      ? `Release ${releaseTag}\n`
      : `Hotfix release ${releaseTag} after ${baseTag}\n`;
  if (
    tag.tagger.name !== expectedName ||
    tag.tagger.email !== expectedEmail ||
    !Number.isFinite(Date.parse(string(tag.tagger.date, "tagger date"))) ||
    tag.message !== expectedMessage
  ) {
    throw new RejectedDelivery("tagger or tag message differs");
  }
}

function validateAsset(raw: unknown): Asset {
  const value = object(raw, "release asset");
  const id = integer(value.id, "asset ID");
  const name = string(value.name, "asset name");
  const url = string(value.url, "asset URL");
  const size = integer(value.size, "asset size");
  const assetDigest = digest(value.digest, "asset digest");
  identity(value.uploader, BOT_LOGIN, BOT_ID, "Bot", "asset uploader");
  if (
    value.state !== "uploaded" ||
    size < 1 ||
    size > MAXIMUM_ASSET_BYTES ||
    url !== `https://api.github.com/repos/${REPOSITORY}/releases/assets/${id}`
  ) {
    throw new RejectedDelivery("release asset identity differs");
  }
  return { raw: value, id, name, url, size, digest: assetDigest };
}

function equalBytes(a: Uint8Array, b: Uint8Array): boolean {
  return a.length === b.length && Buffer.from(a).equals(Buffer.from(b));
}

async function workflowArtifact(
  client: GitHubClient,
  runID: number,
  attempt: number,
  kind: "upstream" | "hotfix",
  receiptBytes: Uint8Array,
  signal: AbortSignal,
  current: boolean,
  expectedHeadSHA: string,
  expected?: { id: number; digest: string; name: string },
): Promise<Map<string, Uint8Array>> {
  const response = object(await client.get(`/repos/${REPOSITORY}/actions/runs/${runID}/artifacts?per_page=100`, signal), "workflow artifacts");
  if (!Array.isArray(response.artifacts) || !Number.isSafeInteger(response.total_count) || response.total_count !== response.artifacts.length)
    throw new RejectedDelivery("workflow artifact listing is malformed");
  const expectedName = `${kind === "upstream" ? "upstream-sync" : "hotfix-release"}-receipt-${runID}-${attempt}`;
  const matches = response.artifacts.filter((raw) => object(raw, "workflow artifact").name === expectedName);
  if (matches.length === 0) {
    if (current) throw new RetryableNotReady("workflow artifact is not visible yet");
    throw new RejectedDelivery("historical workflow artifact is unavailable");
  }
  if (matches.length !== 1) throw new RejectedDelivery("workflow artifact is duplicated");
  const artifact = object(matches[0], "workflow artifact");
  const id = integer(artifact.id, "artifact ID"), size = integer(artifact.size_in_bytes, "artifact size");
  const artifactDigest = digest(artifact.digest, "artifact digest");
  const run = object(artifact.workflow_run, "artifact workflow run");
  if (artifact.name !== expectedName || artifact.expired !== false || size < 1 || size > 4_000_000 ||
      artifact.archive_download_url !== `https://api.github.com/repos/${REPOSITORY}/actions/artifacts/${id}/zip` ||
      run.id !== runID || run.repository_id !== REPOSITORY_ID || run.head_repository_id !== REPOSITORY_ID || run.head_sha !== expectedHeadSHA) {
    if (artifact.expired === true && current) throw new RejectedDelivery("workflow artifact is expired");
    throw new RejectedDelivery("workflow artifact identity differs");
  }
  if (expected && (expected.id !== id || expected.name !== expectedName || expected.digest !== artifactDigest))
    throw new RejectedDelivery("workflow artifact receipt identity differs");
  const zip = await client.bytes(string(artifact.archive_download_url, "artifact archive URL"), signal);
  if (zip.length !== size) throw new RejectedDelivery("workflow artifact size differs");
  if (digestBytes(zip) !== artifactDigest) throw new RejectedDelivery("workflow artifact digest differs");
  let files: Map<string, Uint8Array>;
  try { files = readZipBasenames(zip); } catch { throw new RejectedDelivery("workflow artifact ZIP is invalid"); }
  const receiptName = kind === "upstream" ? "upstream-sync-receipt.json" : "hotfix-release-receipt.json";
  const expectedFiles = kind === "upstream"
    ? [receiptName, "run-state.json"]
    : [receiptName, "independently-verified-receipt.json", "final-plan.out"];
  if ([...files.keys()].sort().join("\n") !== expectedFiles.sort().join("\n"))
    throw new RejectedDelivery("workflow artifact files differ");
  const artifactReceipt = files.get(receiptName);
  if (!artifactReceipt || !equalBytes(artifactReceipt, receiptBytes)) throw new RejectedDelivery("artifact receipt bytes differ");
  if (kind === "hotfix") {
    const independent = files.get("independently-verified-receipt.json");
    if (!independent || !equalBytes(independent, receiptBytes)) throw new RejectedDelivery("independent receipt bytes differ");
  }
  return files;
}

function classifyAssets(release: JSONObject, allowNotReady = true): ReleaseAssets {
  if (!Array.isArray(release.assets))
    throw new RejectedDelivery("release assets differ");
  const all = release.assets.map(validateAsset);
  if (new Set(all.map((asset) => asset.name)).size !== all.length) {
    throw new RejectedDelivery("release asset names are duplicated");
  }

  const receiptNames = new Set([
    "upstream-sync-receipt.json",
    "hotfix-release-receipt.json",
  ]);
  const receipts = all.filter((asset) => receiptNames.has(asset.name));
  const checksums = all.filter((asset) => asset.name === "checksums.txt");
  const archives = all.filter((asset) =>
    /^CLIProxyAPIPlus_[A-Za-z0-9._+-]+\.(?:tar\.gz|zip)$/.test(asset.name),
  );
  const knownCount = receipts.length + checksums.length + archives.length;
  if (receipts.length > 1) {
    throw new RejectedDelivery("release must contain exactly one receipt");
  }
  if (knownCount !== all.length || checksums.length > 1) {
    throw new RejectedDelivery("release asset set differs");
  }
  if (
    checksums.length === 0 ||
    archives.length === 0 ||
    receipts.length === 0
  ) {
    if (!allowNotReady)
      throw new RejectedDelivery("historical release assets are unavailable");
    throw new RetryableNotReady(
      "release publish artifacts are not attached yet",
    );
  }

  return {
    all,
    receipts,
    checksum: checksums[0],
    archives,
    nonReceipt: all.filter((asset) => !receiptNames.has(asset.name)),
  };
}

async function downloadAsset(
  client: GitHubClient,
  asset: Asset,
  signal: AbortSignal,
  maximumBytes: number,
): Promise<Uint8Array> {
  if (asset.size > maximumBytes)
    throw new RejectedDelivery("metadata asset size is invalid");
  const bytes = await client.bytes(asset.url, signal);
  if (bytes.length !== asset.size || digestBytes(bytes) !== asset.digest) {
    throw new RejectedDelivery("asset bytes digest differs");
  }
  return bytes;
}

async function validateChecksums(
  client: GitHubClient,
  assets: ReleaseAssets,
  signal: AbortSignal,
): Promise<void> {
  const bytes = await downloadAsset(
    client,
    assets.checksum,
    signal,
    MAXIMUM_METADATA_BYTES,
  );
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    throw new RejectedDelivery("checksums file is malformed");
  }
  if (text.endsWith("\n")) text = text.slice(0, -1);
  if (!text || text.includes("\r") || text.includes("\n\n")) {
    throw new RejectedDelivery("checksums file is malformed");
  }

  const checksums = new Map<string, string>();
  for (const line of text.split("\n")) {
    const match =
      /^([0-9a-f]{64})  ([A-Za-z0-9][A-Za-z0-9._+-]*\.(?:tar\.gz|zip))$/.exec(
        line,
      );
    if (!match || checksums.has(match[2]))
      throw new RejectedDelivery("checksums file is malformed");
    checksums.set(match[2], `sha256:${match[1]}`);
  }

  if (checksums.size !== assets.archives.length) {
    throw new RejectedDelivery("checksum coverage differs");
  }
  for (const archive of assets.archives) {
    if (checksums.get(archive.name) !== archive.digest) {
      throw new RejectedDelivery("checksum coverage differs");
    }
  }
}

function validateArchitectureImages(value: unknown, tag: string): JSONObject {
  const images = object(value, "architecture images");
  exactKeys(images, ["linux/amd64", "linux/arm64"], "architecture images");
  for (const [platform, suffix] of [
    ["linux/amd64", "amd64"],
    ["linux/arm64", "arm64"],
  ] as const) {
    const image = object(images[platform], "architecture image");
    exactKeys(image, ["image", "digest"], "architecture image");
    if (image.image !== `${IMAGE}:${tag}-${suffix}`) {
      throw new RejectedDelivery("architecture image identity differs");
    }
    digest(image.digest, "architecture image digest");
  }
  return images;
}

function validateReceiptCore(
  receipt: JSONObject,
  release: JSONObject,
  commit: string,
  state: UpstreamState,
  expectedAssets: string[],
): ReceiptCore {
  if (
    receipt.schema_version !== 2 ||
    receipt.sync_id !== state.SYNC_ID ||
    receipt.plan_fingerprint !== state.PLAN_FINGERPRINT ||
    receipt.main_commit !== commit ||
    receipt.tag_commit !== commit ||
    receipt.tag !== release.tag_name ||
    receipt.release_url !== release.html_url ||
    receipt.image !== `${IMAGE}:${release.tag_name}`
  ) {
    throw new RejectedDelivery("receipt core identity differs");
  }
  if (!SHA.test(string(receipt.plan_fingerprint, "receipt fingerprint"))) {
    throw new RejectedDelivery("receipt fingerprint differs");
  }
  compareSortedStrings(
    receipt.release_assets,
    expectedAssets,
    "receipt assets",
  );
  compareSortedStrings(
    receipt.platforms,
    ["linux/amd64", "linux/arm64"],
    "receipt platforms",
  );
  const architectureImages = validateArchitectureImages(
    receipt.architecture_images,
    string(release.tag_name, "release tag"),
  );
  return {
    workflowRunID: decimalInteger(receipt.workflow_run_id, "workflow run ID"),
    imageDigest: digest(receipt.image_digest, "receipt image digest"),
    architectureImages,
  };
}

async function validateWorkflowRun(
  client: GitHubClient,
  runID: number,
  workflowPath: string,
  commit: string,
  signal: AbortSignal,
  options: { hotfix: boolean; allowNotReady: boolean },
): Promise<{ attempt: number; headSHA: string }> {
  const run = object(
    await client.get(`/repos/${REPOSITORY}/actions/runs/${runID}`, signal),
    "workflow run",
  );
  repository(run.repository, "workflow repository");
  identity(run.actor, OWNER_LOGIN, OWNER_ID, "User", "workflow actor");
  if (run.status === "queued" || run.status === "in_progress") {
    if (options.allowNotReady)
      throw new RetryableNotReady("release workflow is not complete");
    throw new RejectedDelivery("release workflow is incomplete");
  }
  if (
    run.path !== workflowPath ||
    run.head_branch !== "main" ||
    run.status !== "completed" ||
    run.conclusion !== "success"
  ) {
    throw new RejectedDelivery("workflow run identity differs");
  }
  const event = string(run.event, "workflow event");
  if (
    options.hotfix
      ? event !== "workflow_dispatch"
      : !["schedule", "workflow_dispatch"].includes(event)
  ) {
    throw new RejectedDelivery("workflow event differs");
  }
  const attempt = integer(run.run_attempt, "workflow run attempt");
  if (attempt < 1) throw new RejectedDelivery("workflow run attempt differs");

  const runHeadSHA = sha(run.head_sha, "workflow head SHA");
  if (options.hotfix) {
    if (run.head_sha !== commit)
      throw new RejectedDelivery("hotfix workflow commit differs");
  } else {
    const comparison = object(
      await client.get(
        `/repos/${REPOSITORY}/compare/${runHeadSHA}...${commit}`,
        signal,
      ),
      "workflow comparison",
    );
    if (comparison.status !== "identical" && comparison.status !== "ahead") {
      throw new RejectedDelivery("workflow head is not an ancestor");
    }
  }
  return { attempt, headSHA: runHeadSHA };
}

function parseManifest(manifest: RegistryManifest, field: string): JSONObject {
  if (
    !DIGEST.test(manifest.digest) ||
    digestBytes(manifest.bytes) !== manifest.digest
  ) {
    throw new RejectedDelivery(`${field} digest differs`);
  }
  return object(parseJSON(manifest.bytes, field), field);
}

export async function verifyRegistry(
  registry: RegistryClient,
  tag: string,
  expectedDigest: string,
  architectureImagesValue: unknown,
  signal: AbortSignal,
  options: { requireLatestParity?: boolean } = {},
): Promise<{ amd64: string; arm64: string }> {
  const architectureImages = validateArchitectureImages(
    architectureImagesValue,
    tag,
  );
  const [tagManifest, latestManifest, amd64Manifest, arm64Manifest] =
    await Promise.all([
      registry.manifest(tag, signal),
      options.requireLatestParity === false
        ? Promise.resolve(undefined)
        : registry.manifest("latest", signal),
      registry.manifest(`${tag}-amd64`, signal),
      registry.manifest(`${tag}-arm64`, signal),
    ]);

  const index = parseManifest(tagManifest, "tag image index");
  const latest = latestManifest
    ? parseManifest(latestManifest, "latest image index")
    : undefined;
  if (
    tagManifest.digest !== expectedDigest ||
    (latestManifest && latestManifest.digest !== expectedDigest) ||
    !INDEX_MEDIA_TYPES.has(tagManifest.mediaType) ||
    (latestManifest && latestManifest.mediaType !== tagManifest.mediaType) ||
    index.schemaVersion !== 2 ||
    index.mediaType !== tagManifest.mediaType ||
    (latest &&
      (latest.schemaVersion !== 2 ||
        latest.mediaType !== latestManifest!.mediaType)) ||
    !Array.isArray(index.manifests)
  ) {
    throw new RejectedDelivery("canonical image index differs");
  }

  const platforms = new Map<string, string>();
  const attestations: string[] = [];
  for (const descriptorValue of index.manifests) {
    const descriptor = object(descriptorValue, "image descriptor");
    const platform = object(descriptor.platform, "image platform");
    exactKeys(platform, ["os", "architecture"], "image platform");
    const descriptorDigest = digest(
      descriptor.digest,
      "image descriptor digest",
    );
    const key = `${platform.os}/${platform.architecture}`;
    if (key === "unknown/unknown") {
      const annotations = object(
        descriptor.annotations,
        "attestation annotations",
      );
      exactKeys(
        annotations,
        ["vnd.docker.reference.digest", "vnd.docker.reference.type"],
        "attestation annotations",
      );
      if (
        descriptor.mediaType !== "application/vnd.oci.image.manifest.v1+json" ||
        annotations["vnd.docker.reference.type"] !== "attestation-manifest" ||
        !DIGEST.test(
          string(
            annotations["vnd.docker.reference.digest"],
            "attestation subject",
          ),
        )
      ) {
        throw new RejectedDelivery(
          "unknown image descriptor is not an attestation",
        );
      }
      attestations.push(
        string(
          annotations["vnd.docker.reference.digest"],
          "attestation subject",
        ),
      );
      continue;
    }
    if (
      !["linux/amd64", "linux/arm64"].includes(key) ||
      platforms.has(key) ||
      !MANIFEST_MEDIA_TYPES.has(
        string(descriptor.mediaType, "image descriptor media type"),
      )
    ) {
      throw new RejectedDelivery("image platform set differs");
    }
    platforms.set(key, descriptorDigest);
  }

  if (platforms.size !== 2)
    throw new RejectedDelivery("image platform set differs");
  if (
    new Set(attestations).size !== attestations.length ||
    attestations.some((subject) => ![...platforms.values()].includes(subject))
  ) {
    throw new RejectedDelivery("attestation subject differs");
  }

  const expectedAMD64 = platforms.get("linux/amd64")!;
  const expectedARM64 = platforms.get("linux/arm64")!;
  for (const [manifest, expected, field] of [
    [amd64Manifest, expectedAMD64, "amd64 image manifest"],
    [arm64Manifest, expectedARM64, "arm64 image manifest"],
  ] as const) {
    const body = parseManifest(manifest, field);
    if (
      manifest.digest !== expected ||
      !MANIFEST_MEDIA_TYPES.has(manifest.mediaType) ||
      body.schemaVersion !== 2 ||
      body.mediaType !== manifest.mediaType
    ) {
      throw new RejectedDelivery("architecture image manifest differs");
    }
  }

  const receiptAMD64 = object(
    architectureImages["linux/amd64"],
    "amd64 receipt image",
  );
  const receiptARM64 = object(
    architectureImages["linux/arm64"],
    "arm64 receipt image",
  );
  if (
    receiptAMD64.digest !== expectedAMD64 ||
    receiptARM64.digest !== expectedARM64
  ) {
    throw new RejectedDelivery("receipt architecture digest differs");
  }
  return { amd64: expectedAMD64, arm64: expectedARM64 };
}

function validateCanonicalReleaseIdentity(
  payloadRelease: JSONObject,
  canonical: JSONObject,
  byTag: JSONObject,
  latest: JSONObject,
  releaseID: number,
  tag: string,
): void {
  const expectedHTMLURL = `https://github.com/${REPOSITORY}/releases/tag/${tag}`;
  const expectedAssetsURL = `https://api.github.com/repos/${REPOSITORY}/releases/${releaseID}/assets`;
  if (payloadRelease.html_url !== expectedHTMLURL || payloadRelease.assets_url !== expectedAssetsURL)
    throw new RejectedDelivery("payload release URL differs");
  for (const [field, expected] of [
    ["id", releaseID],
    ["tag_name", tag],
    ["html_url", expectedHTMLURL],
    ["assets_url", expectedAssetsURL],
    ["published_at", payloadRelease.published_at],
    ["draft", false],
    ["prerelease", false],
    ["target_commitish", "main"],
  ] as const) {
    if (canonical[field] !== expected || byTag[field] !== canonical[field]) {
      throw new RejectedDelivery("release immutable fields differ");
    }
  }
  if (
    payloadRelease.draft !== false ||
    payloadRelease.prerelease !== false ||
    payloadRelease.target_commitish !== "main" ||
    latest.id !== releaseID ||
    latest.tag_name !== tag
  ) {
    throw new RejectedDelivery("release status or latest identity differs");
  }
  identity(canonical.author, BOT_LOGIN, BOT_ID, "Bot", "release author");
  identity(byTag.author, BOT_LOGIN, BOT_ID, "Bot", "tag release author");
  identity(
    payloadRelease.author,
    BOT_LOGIN,
    BOT_ID,
    "Bot",
    "payload release author",
  );
}

function nextHotfixTag(baseTag: string): string {
  const match = /^(v[0-9]+\.[0-9]+\.[0-9]+-unstableneutron\.)([0-9]+)$/.exec(
    baseTag,
  );
  if (!match) throw new RejectedDelivery("previous release tag differs");
  return `${match[1]}${Number(match[2]) + 1}`;
}

interface ReleaseLink {
  tag: string; commit: string;
  receipt: { name: string; id: number; digest: string };
  workflow: { path: string; runID: number; attempt: number; headSHA: string };
  artifact: { id: number; name: string; digest: string };
}

function releaseLink(value: unknown, kind: "upstream" | "hotfix", field: string): ReleaseLink {
  const link = object(value, field); exactKeys(link, RELEASE_LINK_KEYS, field);
  const receipt = object(link.receipt, `${field} receipt`), workflow = object(link.workflow, `${field} workflow`), artifact = object(link.artifact, `${field} artifact`);
  exactKeys(receipt, ["name", "asset_id", "digest"], `${field} receipt`);
  exactKeys(workflow, ["path", "run_id", "run_attempt", "head_sha"], `${field} workflow`);
  exactKeys(artifact, ["id", "name", "digest"], `${field} artifact`);
  const runID = decimalInteger(workflow.run_id, `${field} workflow run ID`), attempt = decimalInteger(workflow.run_attempt, `${field} workflow attempt`);
  const expectedKind = kind === "upstream" ? "upstream-sync" : "hotfix-release";
  const result = { tag: string(link.tag, `${field} tag`), commit: sha(link.commit, `${field} commit`), receipt: { name: string(receipt.name, `${field} receipt name`), id: decimalInteger(receipt.asset_id, `${field} receipt asset ID`), digest: digest(receipt.digest, `${field} receipt digest`) }, workflow: { path: string(workflow.path, `${field} workflow path`), runID, attempt, headSHA: sha(workflow.head_sha, `${field} workflow head SHA`) }, artifact: { id: decimalInteger(artifact.id, `${field} artifact ID`), name: string(artifact.name, `${field} artifact name`), digest: digest(artifact.digest, `${field} artifact digest`) } };
  if (result.receipt.name !== `${expectedKind}-receipt.json` || result.workflow.path !== (kind === "upstream" ? UPSTREAM_WORKFLOW : HOTFIX_WORKFLOW) || result.artifact.name !== `${expectedKind}-receipt-${runID}-${attempt}`)
    throw new RejectedDelivery(`${field} identity differs`);
  return result;
}

async function validatePreviousUpstreamRelease(
  client: GitHubClient,
  registry: RegistryClient,
  baseTag: string,
  baseCommit: string,
  currentStateBytes: Uint8Array,
  currentState: UpstreamState,
  signal: AbortSignal,
  expected?: ReleaseLink,
): Promise<void> {
  const byTag = object(
    await client.get(
      `/repos/${REPOSITORY}/releases/tags/${encodeURIComponent(baseTag)}`,
      signal,
    ),
    "previous release",
  );
  const releaseID = integer(byTag.id, "previous release ID");
  const release = object(
    await client.get(`/repos/${REPOSITORY}/releases/${releaseID}`, signal),
    "canonical previous release",
  );
  if (
    release.id !== releaseID ||
    release.tag_name !== baseTag ||
    byTag.tag_name !== baseTag ||
    release.html_url !== `https://github.com/${REPOSITORY}/releases/tag/${baseTag}` ||
    byTag.html_url !== release.html_url ||
    release.assets_url !== `https://api.github.com/repos/${REPOSITORY}/releases/${releaseID}/assets` ||
    byTag.assets_url !== release.assets_url ||
    byTag.published_at !== release.published_at ||
    release.draft !== false ||
    byTag.draft !== release.draft ||
    release.prerelease !== false ||
    byTag.prerelease !== release.prerelease ||
    release.target_commitish !== "main" ||
    byTag.target_commitish !== release.target_commitish
  ) {
    throw new RejectedDelivery("previous release identity differs");
  }
  identity(release.author, BOT_LOGIN, BOT_ID, "Bot", "previous release author");
  identity(byTag.author, BOT_LOGIN, BOT_ID, "Bot", "previous tag release author");

  const tag = await fetchAnnotatedTag(client, baseTag, signal);
  if (tag.commit !== baseCommit)
    throw new RejectedDelivery("previous tag commit differs");
  validateTagger(tag, "upstream", baseTag);

  const previousState = await readStateAt(client, baseCommit, signal);
  if (
    !Buffer.from(previousState.bytes).equals(Buffer.from(currentStateBytes))
  ) {
    throw new RejectedDelivery("hotfix changed upstream state");
  }
  if (
    previousState.state.EXPECTED_FORK_TAG !== baseTag ||
    previousState.state.SYNC_ID !== currentState.SYNC_ID ||
    previousState.state.PLAN_FINGERPRINT !== currentState.PLAN_FINGERPRINT
  ) {
    throw new RejectedDelivery("previous upstream state differs");
  }

  const assets = classifyAssets(release, false);
  await validateChecksums(client, assets, signal);
  if (
    assets.receipts.length !== 1 ||
    assets.receipts[0].name !== "upstream-sync-receipt.json"
  ) {
    throw new RejectedDelivery("previous release receipt differs");
  }
  const previousReceiptBytes = await downloadAsset(
        client,
        assets.receipts[0],
        signal,
        MAXIMUM_METADATA_BYTES,
      );
  if (expected && (expected.tag !== baseTag || expected.commit !== baseCommit || expected.receipt.id !== assets.receipts[0].id || expected.receipt.name !== assets.receipts[0].name || expected.receipt.digest !== assets.receipts[0].digest))
    throw new RejectedDelivery("recorded upstream root identity differs");
  const receipt = object(
    parseJSON(previousReceiptBytes, "previous receipt"),
    "previous receipt",
  );
  exactKeys(receipt, RECEIPT_CORE_KEYS, "previous receipt");
  const expectedAssets = assets.nonReceipt.map((asset) => asset.name).sort();
  const core = validateReceiptCore(
    receipt,
    release,
    baseCommit,
    previousState.state,
    expectedAssets,
  );
  const previousRun = await validateWorkflowRun(
    client,
    core.workflowRunID,
    UPSTREAM_WORKFLOW,
    baseCommit,
    signal,
    {
      hotfix: false,
      allowNotReady: false,
    },
  );
  if (expected && (expected.workflow.runID !== core.workflowRunID || expected.workflow.attempt !== previousRun.attempt || expected.workflow.headSHA !== previousRun.headSHA))
    throw new RejectedDelivery("recorded upstream root workflow differs");
  const previousArtifact = await workflowArtifact(client, core.workflowRunID, previousRun.attempt, "upstream", previousReceiptBytes, signal, false, previousRun.headSHA, expected?.artifact);
  const runState = previousArtifact.get("run-state.json");
  if (!runState) throw new RejectedDelivery("historical run state is unavailable");
  validateRunState(runState, previousState.state, receipt, baseCommit, baseTag);
  await verifyRegistry(
    registry,
    baseTag,
    core.imageDigest,
    core.architectureImages,
    signal,
    {
      requireLatestParity: false,
    },
  );
}

async function historicalEvidence<T>(operation: () => Promise<T>): Promise<T> {
  try {
    return await operation();
  } catch (error) {
    if (
      error instanceof GitHubHTTPError &&
      (error.status === 404 || error.status === 410)
    ) {
      throw new RejectedDelivery("historical release evidence is unavailable");
    }
    throw error;
  }
}

async function validateHistoricalHotfix(client: GitHubClient, registry: RegistryClient, expected: ReleaseLink, root: ReleaseLink, stateBytes: Uint8Array, state: UpstreamState, signal: AbortSignal, seen: Set<string>, depth: number): Promise<void> {
  return historicalEvidence(() => validateHistoricalHotfixEvidence(client, registry, expected, root, stateBytes, state, signal, seen, depth));
}

async function validateHistoricalHotfixEvidence(client: GitHubClient, registry: RegistryClient, expected: ReleaseLink, root: ReleaseLink, stateBytes: Uint8Array, state: UpstreamState, signal: AbortSignal, seen: Set<string>, depth: number): Promise<void> {
  if (depth >= 32 || seen.has(expected.tag) || seen.has(expected.commit)) throw new RejectedDelivery("hotfix chain cycle or bound exceeded");
  seen.add(expected.tag); seen.add(expected.commit);
  const byTag = object(await client.get(`/repos/${REPOSITORY}/releases/tags/${encodeURIComponent(expected.tag)}`, signal), "historical release");
  const id = integer(byTag.id, "historical release ID"), release = object(await client.get(`/repos/${REPOSITORY}/releases/${id}`, signal), "canonical historical release");
  if (release.id !== id || release.tag_name !== expected.tag || byTag.tag_name !== expected.tag || release.html_url !== `https://github.com/${REPOSITORY}/releases/tag/${expected.tag}` || byTag.html_url !== release.html_url || release.assets_url !== `https://api.github.com/repos/${REPOSITORY}/releases/${id}/assets` || byTag.assets_url !== release.assets_url || release.published_at !== byTag.published_at || release.draft !== false || byTag.draft !== false || release.prerelease !== false || byTag.prerelease !== false || release.target_commitish !== "main" || byTag.target_commitish !== "main") throw new RejectedDelivery("historical release identity differs");
  identity(release.author, BOT_LOGIN, BOT_ID, "Bot", "historical release author"); identity(byTag.author, BOT_LOGIN, BOT_ID, "Bot", "historical tag release author");
  const tag = await fetchAnnotatedTag(client, expected.tag, signal); if (tag.commit !== expected.commit) throw new RejectedDelivery("historical tag commit differs");
  const historicalState = await readStateAt(client, expected.commit, signal); if (!equalBytes(historicalState.bytes, stateBytes) || historicalState.state.SYNC_ID !== state.SYNC_ID || historicalState.state.PLAN_FINGERPRINT !== state.PLAN_FINGERPRINT) throw new RejectedDelivery("hotfix changed upstream state");
  const assets = classifyAssets(release, false); await validateChecksums(client, assets, signal);
  if (assets.receipts[0].name !== "hotfix-release-receipt.json" || assets.receipts[0].id !== expected.receipt.id || assets.receipts[0].digest !== expected.receipt.digest) throw new RejectedDelivery("recorded parent receipt identity differs");
  const receiptBytes = await downloadAsset(client, assets.receipts[0], signal, MAXIMUM_METADATA_BYTES), receipt = object(parseJSON(receiptBytes, "historical receipt"), "historical receipt");
  if (receipt.receipt_type !== "hotfix-release") throw new RejectedDelivery("historical hotfix receipt type differs");
  const version = integer(receipt.hotfix_schema_version, "historical hotfix schema version"); exactKeys(receipt, version === 2 ? HOTFIX_RECEIPT_V2_KEYS : HOTFIX_RECEIPT_KEYS, "historical receipt");
  const core = validateReceiptCore(receipt, release, expected.commit, historicalState.state, assets.nonReceipt.map(a => a.name).sort());
  validateTagger(tag, "hotfix", expected.tag, string(object(receipt.previous_release, "previous release").tag, "previous release tag"));
  const upstreamState = object(receipt.upstream_state, "upstream state receipt"); exactKeys(upstreamState, ["sync_id", "plan_fingerprint", "sha256"], "upstream state receipt"); if (upstreamState.sync_id !== state.SYNC_ID || upstreamState.plan_fingerprint !== state.PLAN_FINGERPRINT || upstreamState.sha256 !== digestBytes(stateBytes).slice(7) || !SHA256.test(string(upstreamState.sha256, "upstream state digest"))) throw new RejectedDelivery("hotfix upstream state receipt differs");
  const assetDigests = object(receipt.release_asset_digests, "release asset digests"), wanted = Object.fromEntries(assets.nonReceipt.map(a => [a.name, a.digest])); exactKeys(assetDigests, Object.keys(wanted), "release asset digests"); if (Object.entries(wanted).some(([name, value]) => assetDigests[name] !== value)) throw new RejectedDelivery("release asset digests differ");
  const workflowReceipt = object(receipt.release_workflow, "release workflow receipt"); exactKeys(workflowReceipt, ["path", "ref", "commit", "run_id", "run_attempt"], "release workflow receipt"); if (workflowReceipt.path !== HOTFIX_WORKFLOW || workflowReceipt.ref !== `${REPOSITORY}/${HOTFIX_WORKFLOW}@refs/heads/main` || workflowReceipt.commit !== expected.commit || decimalInteger(workflowReceipt.run_id, "release workflow run ID") !== core.workflowRunID) throw new RejectedDelivery("release workflow receipt differs");
  const run = await validateWorkflowRun(client, core.workflowRunID, HOTFIX_WORKFLOW, expected.commit, signal, { hotfix: true, allowNotReady: false });
  if (decimalInteger(workflowReceipt.run_attempt, "release workflow attempt") !== run.attempt || expected.workflow.runID !== core.workflowRunID || expected.workflow.attempt !== run.attempt || expected.workflow.headSHA !== run.headSHA) throw new RejectedDelivery("recorded parent workflow differs");
  const files = await workflowArtifact(client, core.workflowRunID, run.attempt, "hotfix", receiptBytes, signal, false, run.headSHA, expected.artifact); const plan = files.get("final-plan.out"); if (!plan) throw new RejectedDelivery("historical final plan is unavailable"); parseFinalPlan(plan, historicalState.state, expected.tag, expected.commit);
  await verifyRegistry(registry, expected.tag, core.imageDigest, core.architectureImages, signal, { requireLatestParity: false });
  const previousRaw = object(receipt.previous_release, "previous release");
  if (version === 1) {
    exactKeys(previousRaw, ["tag", "commit"], "previous release"); const parentTag = string(previousRaw.tag, "previous release tag"), parentCommit = sha(previousRaw.commit, "previous release commit");
    if (!expected.tag.endsWith(".1") || parentTag !== root.tag || parentCommit !== root.commit || nextHotfixTag(parentTag) !== expected.tag) throw new RejectedDelivery("schema-v1 hotfix is only valid at suffix .1");
    const comparison = object(await client.get(`/repos/${REPOSITORY}/compare/${parentCommit}...${expected.commit}`, signal), "historical comparison");
    if (comparison.status !== "ahead") throw new RejectedDelivery("historical ancestry mismatch");
    await validatePreviousUpstreamRelease(client, registry, root.tag, root.commit, stateBytes, state, signal, root);
  } else if (version === 2) {
    if (expected.tag.endsWith(".1")) throw new RejectedDelivery("schema-v2 hotfix requires suffix .2 or later");
    const previous = releaseLink(previousRaw, "hotfix", "previous release"), recordedRoot = releaseLink(receipt.accepted_upstream_root, "upstream", "accepted upstream root");
    if (JSON.stringify(recordedRoot) !== JSON.stringify(root) || nextHotfixTag(previous.tag) !== expected.tag) throw new RejectedDelivery("hotfix parent or root identity differs");
    const comparison = object(await client.get(`/repos/${REPOSITORY}/compare/${previous.commit}...${expected.commit}`, signal), "historical comparison"); if (comparison.status !== "ahead") throw new RejectedDelivery("historical ancestry mismatch");
    if (previous.tag === root.tag) await validatePreviousUpstreamRelease(client, registry, root.tag, root.commit, stateBytes, state, signal, root); else await validateHistoricalHotfix(client, registry, previous, root, stateBytes, state, signal, seen, depth + 1);
  } else throw new RejectedDelivery("hotfix receipt schema differs");
}

export async function validateRelease(
  payloadValue: unknown,
  receivedAt: string,
  client: GitHubClient,
  registry: RegistryClient,
  signal: AbortSignal,
  options: ValidateReleaseOptions = {},
): Promise<VerifiedRelease> {
  const payload = object(payloadValue, "payload");
  if (payload.action !== "published" && payload.action !== "released")
    throw new RejectedDelivery("action is not a stable release publication");
  repository(payload.repository, "payload repository");
  identity(payload.sender, BOT_LOGIN, BOT_ID, "Bot", "sender");

  const payloadRelease = object(payload.release, "payload release");
  const releaseID = integer(payloadRelease.id, "release ID");
  const tag = string(payloadRelease.tag_name, "release tag");
  if (!RELEASE_TAG.test(tag))
    throw new RejectedDelivery("release tag grammar differs");
  const now = (options.now ?? Date.now)();
  validateFreshness(
    string(payloadRelease.published_at, "payload published at"),
    receivedAt,
    now,
  );

  const canonicalRepository = object(
    await client.get(`/repos/${REPOSITORY}`, signal),
    "canonical repository",
  );
  repository(canonicalRepository, "canonical repository");
  if (canonicalRepository.default_branch !== "main") {
    throw new RejectedDelivery("canonical repository default branch differs");
  }

  const [canonical, byTag, latest] = await Promise.all([
    client
      .get(`/repos/${REPOSITORY}/releases/${releaseID}`, signal)
      .then((value) => object(value, "release")),
    client
      .get(
        `/repos/${REPOSITORY}/releases/tags/${encodeURIComponent(tag)}`,
        signal,
      )
      .then((value) => object(value, "tag release")),
    client
      .get(`/repos/${REPOSITORY}/releases/latest`, signal)
      .then((value) => object(value, "latest release")),
  ]);
  validateCanonicalReleaseIdentity(
    payloadRelease,
    canonical,
    byTag,
    latest,
    releaseID,
    tag,
  );
  const publishedAt = string(canonical.published_at, "published at");
  validateFreshness(publishedAt, receivedAt, now);

  const annotatedTag = await fetchAnnotatedTag(client, tag, signal);
  const commit = annotatedTag.commit;
  const [commitObject, main, mainComparison] = await Promise.all([
    client
      .get(`/repos/${REPOSITORY}/commits/${commit}`, signal)
      .then((value) => object(value, "commit")),
    client
      .get(`/repos/${REPOSITORY}/commits/main`, signal)
      .then((value) => object(value, "main")),
    client
      .get(`/repos/${REPOSITORY}/compare/${commit}...main`, signal)
      .then((value) => object(value, "main comparison")),
  ]);
  if (
    commitObject.sha !== commit ||
    main.sha !== commit ||
    mainComparison.status !== "identical"
  ) {
    throw new RejectedDelivery("tag, commit, or main identity differs");
  }

  const currentState = await readStateAt(client, commit, signal);
  const assets = classifyAssets(canonical);
  await validateChecksums(client, assets, signal);
  if (assets.receipts.length === 0) {
    throw new RetryableNotReady("release receipt is not attached yet");
  }
  if (assets.receipts.length !== 1) {
    throw new RejectedDelivery("release must contain exactly one receipt");
  }

  const receiptAsset = assets.receipts[0];
  const receiptBytes = await downloadAsset(client, receiptAsset, signal, MAXIMUM_METADATA_BYTES);
  const receipt = object(
    parseJSON(
      receiptBytes,
      "release receipt",
    ),
    "release receipt",
  );
  const kind =
    receiptAsset.name === "upstream-sync-receipt.json" ? "upstream" : "hotfix";
  exactKeys(
    receipt,
    kind === "upstream" ? RECEIPT_CORE_KEYS : receipt.hotfix_schema_version === 2 ? HOTFIX_RECEIPT_V2_KEYS : HOTFIX_RECEIPT_KEYS,
    "release receipt",
  );

  const expectedAssets = assets.nonReceipt.map((asset) => asset.name).sort();
  const core = validateReceiptCore(
    receipt,
    canonical,
    commit,
    currentState.state,
    expectedAssets,
  );
  let workflowRunAttempt: number;

  if (kind === "upstream") {
    if (currentState.state.EXPECTED_FORK_TAG !== tag) {
      throw new RejectedDelivery("upstream state tag differs");
    }
    validateTagger(annotatedTag, "upstream", tag);
    const workflowRun = await validateWorkflowRun(
      client,
      core.workflowRunID,
      UPSTREAM_WORKFLOW,
      commit,
      signal,
      { hotfix: false, allowNotReady: true },
    );
    workflowRunAttempt = workflowRun.attempt;
    const artifact = await workflowArtifact(client, core.workflowRunID, workflowRunAttempt, "upstream", receiptBytes, signal, true, workflowRun.headSHA);
    const runState = artifact.get("run-state.json");
    if (!runState) throw new RejectedDelivery("run state artifact is missing");
    validateRunState(runState, currentState.state, receipt, commit, tag);
  } else {
    if (
      receipt.receipt_type !== "hotfix-release" ||
      (receipt.hotfix_schema_version !== 1 && receipt.hotfix_schema_version !== 2)
    ) {
      throw new RejectedDelivery("hotfix receipt schema differs");
    }
    const previous = object(receipt.previous_release, "previous release");
    let baseTag: string, baseCommit: string, parentLink: ReleaseLink | undefined, rootLink: ReleaseLink | undefined;
    if (receipt.hotfix_schema_version === 1) {
      exactKeys(previous, ["tag", "commit"], "previous release");
      baseTag = string(previous.tag, "previous release tag"); baseCommit = sha(previous.commit, "previous release commit");
      if (!tag.endsWith(".1")) throw new RejectedDelivery("schema-v1 hotfix is only valid at suffix .1");
    } else {
      const suffix = Number(tag.slice(tag.lastIndexOf(".") + 1));
      if (suffix < 2) throw new RejectedDelivery("schema-v2 hotfix requires suffix .2 or later");
      parentLink = releaseLink(previous, "hotfix", "previous release"); rootLink = releaseLink(receipt.accepted_upstream_root, "upstream", "accepted upstream root");
      baseTag = parentLink.tag; baseCommit = parentLink.commit;
      if (rootLink.tag !== currentState.state.EXPECTED_FORK_TAG || !rootLink.tag.endsWith(".0") || !tag.startsWith(rootLink.tag.slice(0, -1))) throw new RejectedDelivery("accepted upstream root identity differs");
    }
    if (
      !RELEASE_TAG.test(baseTag) ||
      nextHotfixTag(baseTag) !== tag ||
      (receipt.hotfix_schema_version === 1 ? currentState.state.EXPECTED_FORK_TAG !== baseTag : false)
    ) {
      throw new RejectedDelivery("hotfix tag relationship differs");
    }
    validateTagger(annotatedTag, "hotfix", tag, baseTag);

    const upstreamState = object(
      receipt.upstream_state,
      "upstream state receipt",
    );
    exactKeys(
      upstreamState,
      ["sync_id", "plan_fingerprint", "sha256"],
      "upstream state receipt",
    );
    if (
      upstreamState.sync_id !== currentState.state.SYNC_ID ||
      upstreamState.plan_fingerprint !== currentState.state.PLAN_FINGERPRINT ||
      upstreamState.sha256 !==
        digestBytes(currentState.bytes).slice("sha256:".length) ||
      !SHA256.test(string(upstreamState.sha256, "upstream state digest"))
    ) {
      throw new RejectedDelivery("hotfix upstream state receipt differs");
    }

    const assetDigests = object(
      receipt.release_asset_digests,
      "release asset digests",
    );
    const expectedDigests = Object.fromEntries(
      assets.nonReceipt.map((asset) => [asset.name, asset.digest]),
    );
    exactKeys(
      assetDigests,
      Object.keys(expectedDigests),
      "release asset digests",
    );
    if (
      Object.entries(expectedDigests).some(
        ([name, expected]) => assetDigests[name] !== expected,
      )
    ) {
      throw new RejectedDelivery("release asset digests differ");
    }

    const releaseWorkflow = object(
      receipt.release_workflow,
      "release workflow receipt",
    );
    exactKeys(
      releaseWorkflow,
      ["path", "ref", "commit", "run_id", "run_attempt"],
      "release workflow receipt",
    );
    const releaseWorkflowRunID = decimalInteger(
      releaseWorkflow.run_id,
      "release workflow run ID",
    );
    const releaseWorkflowAttempt = decimalInteger(
      releaseWorkflow.run_attempt,
      "release workflow run attempt",
    );
    if (
      core.workflowRunID !== releaseWorkflowRunID ||
      releaseWorkflow.path !== HOTFIX_WORKFLOW ||
      releaseWorkflow.ref !==
        `${REPOSITORY}/${HOTFIX_WORKFLOW}@refs/heads/main` ||
      releaseWorkflow.commit !== commit
    ) {
      throw new RejectedDelivery("release workflow receipt differs");
    }

    const baseComparison = object(
      await client.get(
        `/repos/${REPOSITORY}/compare/${baseCommit}...${commit}`,
        signal,
      ),
      "hotfix base comparison",
    );
    if (baseComparison.status !== "ahead") {
      throw new RejectedDelivery(
        "hotfix commit is not strictly ahead of its base",
      );
    }
    if (receipt.hotfix_schema_version === 1) await historicalEvidence(() => validatePreviousUpstreamRelease(client, registry, baseTag, baseCommit, currentState.bytes, currentState.state, signal));
    else if (parentLink!.tag === rootLink!.tag) await historicalEvidence(() => validatePreviousUpstreamRelease(client, registry, rootLink!.tag, rootLink!.commit, currentState.bytes, currentState.state, signal, rootLink));
    else await validateHistoricalHotfix(client, registry, parentLink!, rootLink!, currentState.bytes, currentState.state, signal, new Set([tag, commit]), 1);
    const workflowRun = await validateWorkflowRun(
      client,
      core.workflowRunID,
      HOTFIX_WORKFLOW,
      commit,
      signal,
      { hotfix: true, allowNotReady: true },
    );
    workflowRunAttempt = workflowRun.attempt;
    if (workflowRunAttempt !== releaseWorkflowAttempt) {
      throw new RejectedDelivery("release workflow attempt differs");
    }
    const artifact = await workflowArtifact(client, core.workflowRunID, workflowRunAttempt, "hotfix", receiptBytes, signal, true, workflowRun.headSHA);
    const finalPlan = artifact.get("final-plan.out");
    if (!finalPlan) throw new RejectedDelivery("final plan artifact is missing");
    parseFinalPlan(finalPlan, currentState.state, tag, commit);
  }

  const architectures = await verifyRegistry(
    registry,
    tag,
    core.imageDigest,
    core.architectureImages,
    signal,
  );
  return {
    repository: REPOSITORY,
    repositoryID: REPOSITORY_ID,
    releaseID,
    releaseURL: string(canonical.html_url, "release URL"),
    tag,
    publishedAt,
    commit,
    kind,
    syncID: currentState.state.SYNC_ID,
    planFingerprint: currentState.state.PLAN_FINGERPRINT,
    imageDigest: core.imageDigest,
    architectures,
    workflowPath: kind === "upstream" ? UPSTREAM_WORKFLOW : HOTFIX_WORKFLOW,
    workflowRunID: core.workflowRunID,
    workflowRunAttempt,
    revalidation: { payload: structuredClone(payloadValue), receivedAt, now: options.now ?? Date.now },
  };
}

/** Re-sample every mutable source immediately before a durable claim. */
export async function revalidateRelease(
  verified: VerifiedRelease,
  client: GitHubClient,
  registry: RegistryClient,
  signal: AbortSignal,
): Promise<void> {
  let current: VerifiedRelease;
  try {
    current = await validateRelease(verified.revalidation.payload, verified.revalidation.receivedAt, client, registry, signal, { now: verified.revalidation.now });
  } catch (error) {
    if (error instanceof RetryableNotReady)
      throw new RejectedDelivery("release snapshot drifted before claim");
    throw error;
  }
  const snapshot = ({ revalidation: _ignored, ...value }: VerifiedRelease) => value;
  if (JSON.stringify(snapshot(current)) !== JSON.stringify(snapshot(verified)))
    throw new RejectedDelivery("release snapshot drifted before claim");
}
