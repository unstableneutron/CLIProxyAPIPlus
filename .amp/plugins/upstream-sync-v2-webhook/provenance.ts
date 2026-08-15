import { createHash, createHmac, timingSafeEqual } from 'node:crypto'

export const repository = 'unstableneutron/CLIProxyAPIPlus'
export const repositoryID = 1247056725
export const repositoryOwnerID = 156744497
export const githubActionsBotID = 41898282
export const githubActionsBotLogin = 'github-actions[bot]'

const candidatePrefix = 'upstream-sync/'
const workflowPath = '.github/workflows/upstream-sync-v2.yml'
const sourceRunMaximumAgeMs = 12 * 60 * 60 * 1000

export interface GitHubClient {
  get(path: string, signal: AbortSignal, allowNotFound?: boolean): Promise<unknown | null>
}

export interface Candidate {
  repository: string
  repositoryID: number
  number: number
  url: string
  action: 'opened' | 'reopened'
  headRef: string
  headSHA: string
  baseSHA: string
  syncID: string
  planFingerprint: string
  expectedTag: string
  originalTag: string
  originalCommit: string
  plusTag: string
  plusTagCommit: string
  plusHeadCommit: string
  plusHeadIncluded: boolean
  modelsCommit: string
  workflowRunID: number
  supersededPRs: number[]
}

interface ParsedBody {
  syncID: string
  candidateBranch: string
  expectedTag: string
  planFingerprint: string
  workflowRunID: number
  baseCommit: string
  originalTag: string
  originalCommit: string
  plusTag: string
  plusTagCommit: string
  plusHeadCommit: string
  plusHeadIncluded: boolean
  modelsCommit: string
}

type JSONRecord = Record<string, unknown>

export class RejectedDelivery extends Error {}

export function verifyGitHubSignature(body: Uint8Array, signature: string | undefined, secret: string): boolean {
  if (!signature?.startsWith('sha256=') || secret.length < 32) return false
  const expected = Buffer.from(createHmac('sha256', secret).update(body).digest('hex'), 'ascii')
  const supplied = Buffer.from(signature.slice('sha256='.length), 'ascii')
  return supplied.length === expected.length && timingSafeEqual(supplied, expected)
}

export function gitBlobHash(content: string): string {
  const bytes = Buffer.from(content)
  return createHash('sha1').update(`blob ${bytes.length}\0`).update(bytes).digest('hex')
}

export function planFingerprint(fields: Omit<ParsedBody, 'syncID' | 'candidateBranch' | 'planFingerprint' | 'workflowRunID'>): string {
  const content = [
    `base_fork_commit=${fields.baseCommit}`,
    `original_tag=${fields.originalTag}`,
    `original_commit=${fields.originalCommit}`,
    `plus_tag=${fields.plusTag}`,
    `plus_tag_commit=${fields.plusTagCommit}`,
    `plus_head_commit=${fields.plusHeadCommit}`,
    `plus_head_included=${fields.plusHeadIncluded}`,
    `models_commit=${fields.modelsCommit}`,
    `expected_fork_tag=${fields.expectedTag}`,
    '',
  ].join('\n')
  return gitBlobHash(content)
}

function record(value: unknown, field: string): JSONRecord {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new RejectedDelivery(`${field} is not an object`)
  }
  return value as JSONRecord
}

function string(value: unknown, field: string): string {
  if (typeof value !== 'string' || !value) throw new RejectedDelivery(`${field} is missing`)
  return value
}

function integer(value: unknown, field: string): number {
  if (!Number.isSafeInteger(value)) throw new RejectedDelivery(`${field} is not an integer`)
  return value as number
}

function boolean(value: unknown, field: string): boolean {
  if (typeof value !== 'boolean') throw new RejectedDelivery(`${field} is not a boolean`)
  return value
}

function exactMatch(body: string, expression: RegExp, field: string): RegExpMatchArray {
  const flags = expression.flags.includes('g') ? expression.flags : `${expression.flags}g`
  const matches = [...body.matchAll(new RegExp(expression.source, flags))]
  if (matches.length !== 1) {
    throw new RejectedDelivery(`candidate body must contain exactly one ${field}`)
  }
  return matches[0]
}

export function parseCandidateBody(body: string): ParsedBody {
  if (body.length > 100_000) throw new RejectedDelivery('candidate body is too large')
  const syncID = exactMatch(body, /^- Sync ID: `([^`]+)`$/m, 'sync ID')[1]
  const candidateBranch = exactMatch(body, /^- Candidate branch: `([^`]+)`$/m, 'candidate branch')[1]
  const expectedTag = exactMatch(body, /^- Expected fork tag: `([^`]+)`$/m, 'expected fork tag')[1]
  const planFingerprintValue = exactMatch(body, /^- Plan fingerprint: `([0-9a-f]{40})`$/m, 'plan fingerprint')[1]
  const workflowRunID = Number(exactMatch(
    body,
    /^- Workflow: \[https:\/\/github\.com\/unstableneutron\/CLIProxyAPIPlus\/actions\/runs\/(\d+)\]\(https:\/\/github\.com\/unstableneutron\/CLIProxyAPIPlus\/actions\/runs\/\1\)$/m,
    'workflow run',
  )[1])
  const baseCommit = exactMatch(body, /^\| Fork base \| `main` \| `([0-9a-f]{40})` \|$/m, 'fork base')[1]
  const original = exactMatch(body, /^\| Original \| `(v[^`]+)` \| `([0-9a-f]{40})` \|$/m, 'Original snapshot')
  const plus = exactMatch(body, /^\| Plus release \| `(v[^`]+)` \| `([0-9a-f]{40})` \|$/m, 'Plus release snapshot')
  const plusHead = exactMatch(body, /^\| Plus head \(included: `(true|false)`\) \| `main` \| `([0-9a-f]{40})` \|$/m, 'Plus head snapshot')
  const modelsCommit = exactMatch(body, /^\| Models \| `main` \| `([0-9a-f]{40})` \|$/m, 'models snapshot')[1]
  exactMatch(body, /^- Fresh: \*\*true\*\*$/m, 'fresh status')
  exactMatch(body, /^- Conflicts: \*\*None\*\*$/m, 'conflict status')

  return {
    syncID,
    candidateBranch,
    expectedTag,
    planFingerprint: planFingerprintValue,
    workflowRunID,
    baseCommit,
    originalTag: original[1],
    originalCommit: original[2],
    plusTag: plus[1],
    plusTagCommit: plus[2],
    plusHeadCommit: plusHead[2],
    plusHeadIncluded: plusHead[1] === 'true',
    modelsCommit,
  }
}

function requireIdentity(value: unknown, login: string, id: number, type: string, field: string): void {
  const identity = record(value, field)
  if (identity.login !== login || identity.id !== id || identity.type !== type) {
    throw new RejectedDelivery(`${field} identity does not match`)
  }
}

function requireAllowedSender(value: unknown): void {
  const identity = record(value, 'sender')
  const isBot = identity.login === githubActionsBotLogin && identity.id === githubActionsBotID && identity.type === 'Bot'
  const isOwner = identity.login === 'unstableneutron' && identity.id === repositoryOwnerID && identity.type === 'User'
  if (!isBot && !isOwner) throw new RejectedDelivery('sender is not permitted to open or reopen a candidate')
}

function requireSHA(value: unknown, expected: string, field: string): void {
  if (string(value, field) !== expected) throw new RejectedDelivery(`${field} moved`)
}

function sha(value: unknown, field: string): string {
  const result = string(value, field)
  if (!/^[0-9a-f]{40}$/.test(result)) throw new RejectedDelivery(`${field} is not a lowercase 40-character SHA`)
  return result
}

function pathForCommit(repo: string, ref: string): string {
  return `/repos/${repo}/commits/${encodeURIComponent(ref)}`
}

async function currentCommit(client: GitHubClient, repo: string, ref: string, signal: AbortSignal): Promise<string> {
  const commit = record(await client.get(pathForCommit(repo, ref), signal), `${repo}@${ref}`)
  return sha(commit.sha, `${repo}@${ref} SHA`)
}

async function latestRelease(client: GitHubClient, repo: string, signal: AbortSignal): Promise<string> {
  const release = record(await client.get(`/repos/${repo}/releases/latest`, signal), `${repo} latest release`)
  return string(release.tag_name, `${repo} latest release tag`)
}

export async function validateCandidate(
  payloadValue: unknown,
  receivedAt: string,
  client: GitHubClient,
  signal: AbortSignal,
): Promise<Candidate> {
  const payload = record(payloadValue, 'payload')
  const action = string(payload.action, 'action')
  if (action !== 'opened' && action !== 'reopened') throw new RejectedDelivery(`unsupported action ${action}`)
  requireAllowedSender(payload.sender)

  const payloadRepository = record(payload.repository, 'repository')
  if (payloadRepository.full_name !== repository || payloadRepository.id !== repositoryID) {
    throw new RejectedDelivery('repository identity does not match')
  }
  const number = integer(payload.number, 'pull request number')
  const payloadPR = record(payload.pull_request, 'pull request')
  if (integer(payloadPR.number, 'payload pull request number') !== number) {
    throw new RejectedDelivery('pull request number mismatch')
  }

  const canonical = record(await client.get(`/repos/${repository}/pulls/${number}`, signal), 'canonical pull request')
  if (canonical.state !== 'open' || boolean(canonical.draft, 'draft')) {
    throw new RejectedDelivery('pull request is not open and reviewable')
  }
  requireIdentity(canonical.user, githubActionsBotLogin, githubActionsBotID, 'Bot', 'pull request author')
  const base = record(canonical.base, 'base')
  const head = record(canonical.head, 'head')
  const baseRepo = record(base.repo, 'base repository')
  const headRepo = record(head.repo, 'head repository')
  if (base.ref !== 'main' || baseRepo.full_name !== repository || baseRepo.id !== repositoryID) {
    throw new RejectedDelivery('base provenance does not match')
  }
  if (headRepo.full_name !== repository || headRepo.id !== repositoryID) {
    throw new RejectedDelivery('head must belong to the fork repository')
  }
  requireIdentity(head.user, 'unstableneutron', repositoryOwnerID, 'User', 'head owner')

  const headRef = string(head.ref, 'head ref')
  const headSHA = sha(head.sha, 'head SHA')
  const baseSHA = sha(base.sha, 'base SHA')
  const body = parseCandidateBody(string(canonical.body, 'pull request body'))
  const expectedBranch = `${candidatePrefix}${body.syncID}-${body.planFingerprint.slice(0, 12)}`
  if (headRef !== expectedBranch || body.candidateBranch !== expectedBranch) {
    throw new RejectedDelivery('candidate branch does not match fingerprint')
  }
  if (canonical.title !== `Resolve upstream sync ${body.expectedTag}`) {
    throw new RejectedDelivery('candidate title does not match expected tag')
  }
  if (body.syncID !== `original-${body.originalTag}_plus-${body.plusTag}`) {
    throw new RejectedDelivery('sync ID does not match source tags')
  }
  if (!/^v[0-9A-Za-z._-]+$/.test(body.originalTag) || !/^v[0-9A-Za-z._-]+$/.test(body.plusTag)) {
    throw new RejectedDelivery('source tag grammar does not match')
  }
  if (!/^v[0-9]+\.[0-9]+\.[0-9]+-unstableneutron\.[0-9]+$/.test(body.expectedTag)) {
    throw new RejectedDelivery('expected fork tag grammar does not match')
  }
  if (body.baseCommit !== baseSHA) throw new RejectedDelivery('body fork base does not match pull request base')
  if (planFingerprint(body) !== body.planFingerprint) throw new RejectedDelivery('plan fingerprint does not recompute')

  const [currentMain, originalRelease, plusRelease] = await Promise.all([
    currentCommit(client, repository, 'main', signal),
    latestRelease(client, 'router-for-me/CLIProxyAPI', signal),
    latestRelease(client, 'kaitranntt/CLIProxyAPIPlus', signal),
  ])
  requireSHA(currentMain, baseSHA, 'fork main')
  if (originalRelease !== body.originalTag) throw new RejectedDelivery('Original release tag is no longer latest')
  if (plusRelease !== body.plusTag) throw new RejectedDelivery('Plus release tag is no longer latest')
  requireSHA(await currentCommit(client, 'router-for-me/CLIProxyAPI', body.originalTag, signal), body.originalCommit, 'Original tag')
  requireSHA(await currentCommit(client, 'kaitranntt/CLIProxyAPIPlus', body.plusTag, signal), body.plusTagCommit, 'Plus tag')
  requireSHA(await currentCommit(client, 'kaitranntt/CLIProxyAPIPlus', 'main', signal), body.plusHeadCommit, 'Plus head')
  requireSHA(await currentCommit(client, 'router-for-me/models', 'main', signal), body.modelsCommit, 'models head')

  const existingTag = await client.get(`/repos/${repository}/git/ref/tags/${encodeURIComponent(body.expectedTag)}`, signal, true)
  if (existingTag !== null) throw new RejectedDelivery('expected fork tag already exists')

  const run = record(await client.get(`/repos/${repository}/actions/runs/${body.workflowRunID}`, signal), 'workflow run')
  const runRepository = record(run.repository, 'workflow repository')
  if (
    runRepository.id !== repositoryID ||
    run.path !== workflowPath ||
    run.event !== 'schedule' ||
    run.head_branch !== 'main' ||
    run.head_sha !== baseSHA
  ) {
    throw new RejectedDelivery('workflow run provenance does not match the daily v2 planner')
  }
  const received = Date.parse(receivedAt)
  const runCreated = Date.parse(string(run.created_at, 'workflow creation time'))
  if (!Number.isFinite(received) || !Number.isFinite(runCreated) || received < runCreated || received - runCreated > sourceRunMaximumAgeMs) {
    throw new RejectedDelivery('workflow run is not fresh')
  }

  const openPullsValue = await client.get(`/repos/${repository}/pulls?state=open&base=main&per_page=100`, signal)
  if (!Array.isArray(openPullsValue)) throw new RejectedDelivery('open pull request list is invalid')
  if (openPullsValue.length === 100) throw new RejectedDelivery('open pull request list is not provably complete')
  const candidateCreated = Date.parse(string(canonical.created_at, 'candidate creation time'))
  if (!Number.isFinite(candidateCreated)) throw new RejectedDelivery('candidate creation time is invalid')
  const supersededPRs: number[] = []
  for (const value of openPullsValue) {
    const pull = record(value, 'open pull request')
    const pullHead = record(pull.head, 'open pull request head')
    const pullUser = record(pull.user, 'open pull request user')
    if (
      typeof pullHead.ref !== 'string' ||
      !pullHead.ref.startsWith(candidatePrefix) ||
      pullUser.login !== githubActionsBotLogin ||
      pullUser.id !== githubActionsBotID
    ) continue
    const pullNumber = integer(pull.number, 'open candidate number')
    if (pullNumber === number) continue
    const created = Date.parse(string(pull.created_at, 'open candidate creation time'))
    if (!Number.isFinite(created)) throw new RejectedDelivery('open candidate creation time is invalid')
    if (created > candidateCreated) throw new RejectedDelivery(`newer upstream-sync candidate PR #${pullNumber} exists`)
    supersededPRs.push(pullNumber)
  }

  return {
    repository,
    repositoryID,
    number,
    url: string(canonical.html_url, 'pull request URL'),
    action: action as Candidate['action'],
    headRef,
    headSHA,
    baseSHA,
    syncID: body.syncID,
    planFingerprint: body.planFingerprint,
    expectedTag: body.expectedTag,
    originalTag: body.originalTag,
    originalCommit: body.originalCommit,
    plusTag: body.plusTag,
    plusTagCommit: body.plusTagCommit,
    plusHeadCommit: body.plusHeadCommit,
    plusHeadIncluded: body.plusHeadIncluded,
    modelsCommit: body.modelsCommit,
    workflowRunID: body.workflowRunID,
    supersededPRs: supersededPRs.sort((a, b) => a - b),
  }
}
