import { describe, expect, test } from 'bun:test'
import { createHmac } from 'node:crypto'
import {
  githubActionsBotID,
  githubActionsBotLogin,
  planFingerprint,
  repository,
  repositoryID,
  repositoryOwnerID,
  validateCandidate,
  verifyGitHubSignature,
  type GitHubClient,
} from './provenance'

const signal = new AbortController().signal
const receivedAt = '2026-08-15T05:55:00Z'
const baseSHA = '1111111111111111111111111111111111111111'
const originalCommit = '2222222222222222222222222222222222222222'
const plusCommit = '3333333333333333333333333333333333333333'
const modelsCommit = '4444444444444444444444444444444444444444'
const headSHA = '5555555555555555555555555555555555555555'
const originalTag = 'v7.2.133'
const plusTag = 'v7.2.127-4'
const expectedTag = 'v7.2.133-unstableneutron.0'
const syncID = `original-${originalTag}_plus-${plusTag}`
const fingerprint = planFingerprint({
  baseCommit: baseSHA,
  originalTag,
  originalCommit,
  plusTag,
  plusTagCommit: plusCommit,
  plusHeadCommit: plusCommit,
  plusHeadIncluded: false,
  modelsCommit,
  expectedTag,
})
const branch = `upstream-sync/${syncID}-${fingerprint.slice(0, 12)}`

const bot = { login: githubActionsBotLogin, id: githubActionsBotID, type: 'Bot' }
const owner = { login: 'unstableneutron', id: repositoryOwnerID, type: 'User' }
const repo = { full_name: repository, id: repositoryID }

const body = `# Upstream sync candidate

- Sync ID: \`${syncID}\`
- Candidate branch: \`${branch}\`
- Expected fork tag: \`${expectedTag}\`
- Plan fingerprint: \`${fingerprint}\`
- Workflow: [https://github.com/unstableneutron/CLIProxyAPIPlus/actions/runs/12345](https://github.com/unstableneutron/CLIProxyAPIPlus/actions/runs/12345)

## Exact snapshot

| Source | Ref | Commit |
|---|---|---|
| Fork base | \`main\` | \`${baseSHA}\` |
| Original | \`${originalTag}\` | \`${originalCommit}\` |
| Plus release | \`${plusTag}\` | \`${plusCommit}\` |
| Plus head (included: \`false\`) | \`main\` | \`${plusCommit}\` |
| Models | \`main\` | \`${modelsCommit}\` |

## Freshness and conflicts

- Fresh: **true**
- Stale reasons: None
- Conflicts: **None**
`

function fixture() {
  const canonical = {
    number: 77,
    state: 'open',
    draft: false,
    user: bot,
    title: `Resolve upstream sync ${expectedTag}`,
    body,
    html_url: 'https://github.com/unstableneutron/CLIProxyAPIPlus/pull/77',
    created_at: '2026-08-15T05:40:00Z',
    base: { ref: 'main', sha: baseSHA, repo, user: owner },
    head: { ref: branch, sha: headSHA, repo, user: owner },
  }
  const payload = {
    action: 'opened',
    number: 77,
    sender: bot,
    repository: repo,
    pull_request: { number: 77 },
  }
  const values = new Map<string, unknown>([
    [`/repos/${repository}/pulls/77`, canonical],
    [`/repos/${repository}/commits/main`, { sha: baseSHA }],
    ['/repos/router-for-me/CLIProxyAPI/releases/latest', { tag_name: originalTag }],
    ['/repos/kaitranntt/CLIProxyAPIPlus/releases/latest', { tag_name: plusTag }],
    ['/repos/router-for-me/CLIProxyAPI/commits/v7.2.133', { sha: originalCommit }],
    ['/repos/kaitranntt/CLIProxyAPIPlus/commits/v7.2.127-4', { sha: plusCommit }],
    ['/repos/kaitranntt/CLIProxyAPIPlus/commits/main', { sha: plusCommit }],
    ['/repos/router-for-me/models/commits/main', { sha: modelsCommit }],
    [`/repos/${repository}/actions/runs/12345`, {
      repository: { id: repositoryID },
      path: '.github/workflows/upstream-sync-v2.yml',
      event: 'schedule',
      head_branch: 'main',
      head_sha: baseSHA,
      created_at: '2026-08-15T05:35:00Z',
    }],
    [`/repos/${repository}/pulls?state=open&base=main&per_page=100`, [canonical]],
  ])
  const client: GitHubClient = {
    get: async (path, _signal, allowNotFound) => {
      if (values.has(path)) return structuredClone(values.get(path))
      if (allowNotFound) return null
      throw new Error(`missing fixture: ${path}`)
    },
  }
  return { payload, canonical, client, values }
}

describe('GitHub signature verification', () => {
  test('accepts the exact body signature and rejects drift', () => {
    const secret = 'a'.repeat(32)
    const message = new TextEncoder().encode('payload')
    const signature = `sha256=${createHmac('sha256', secret).update(message).digest('hex')}`
    expect(verifyGitHubSignature(message, signature, secret)).toBe(true)
    expect(verifyGitHubSignature(new TextEncoder().encode('changed'), signature, secret)).toBe(false)
  })
})

describe('candidate provenance', () => {
  test('accepts an exact fresh scheduled candidate', async () => {
    const { payload, client } = fixture()
    const candidate = await validateCandidate(payload, receivedAt, client, signal)
    expect(candidate.number).toBe(77)
    expect(candidate.planFingerprint).toBe(fingerprint)
    expect(candidate.headRef).toBe(branch)
  })

  test('rejects a fingerprint that does not match immutable inputs', async () => {
    const { payload, client, values } = fixture()
    const canonical = values.get(`/repos/${repository}/pulls/77`) as { body: string }
    canonical.body = canonical.body.replace(`Plan fingerprint: \`${fingerprint}\``, `Plan fingerprint: \`${'f'.repeat(40)}\``)
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('candidate branch does not match fingerprint')
  })

  test('rejects workflow_dispatch candidates instead of treating them as daily', async () => {
    const { payload, client, values } = fixture()
    const run = values.get(`/repos/${repository}/actions/runs/12345`) as { event: string }
    run.event = 'workflow_dispatch'
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('daily v2 planner')
  })

  test('rejects stale source heads', async () => {
    const { payload, client, values } = fixture()
    values.set('/repos/router-for-me/models/commits/main', { sha: '9'.repeat(40) })
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('models head moved')
  })

  test('rejects a source tag that is no longer the latest release', async () => {
    const { payload, client, values } = fixture()
    values.set('/repos/router-for-me/CLIProxyAPI/releases/latest', { tag_name: 'v7.2.134' })
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('Original release tag is no longer latest')
  })

  test('rejects a candidate when a newer candidate is open', async () => {
    const { payload, client, values } = fixture()
    const openPulls = values.get(`/repos/${repository}/pulls?state=open&base=main&per_page=100`) as unknown[]
    openPulls.push({
      number: 78,
      created_at: '2026-08-15T05:41:00Z',
      user: bot,
      head: { ref: `${branch}-newer` },
    })
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('newer upstream-sync candidate')
  })

  test('accepts a repository-owner reopened delivery for the bot-authored candidate', async () => {
    const { payload, client } = fixture()
    payload.action = 'reopened'
    payload.sender = owner
    expect((await validateCandidate(payload, receivedAt, client, signal)).action).toBe('reopened')
  })

  test('rejects unsupported pull request actions', async () => {
    const { payload, client } = fixture()
    payload.action = 'synchronize'
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('unsupported action')
  })

  test('rejects repository identity spoofing', async () => {
    const { payload, client } = fixture()
    payload.repository = { ...repo, id: repositoryID + 1 }
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('repository identity')
  })

  test('rejects a non-bot canonical author', async () => {
    const { payload, canonical, client } = fixture()
    canonical.user = owner
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('pull request author identity')
  })

  test('rejects a candidate head from another repository', async () => {
    const { payload, canonical, client } = fixture()
    canonical.head.repo = { full_name: 'attacker/fork', id: 42 }
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('head must belong')
  })

  test('rejects a title inconsistent with the expected tag', async () => {
    const { payload, canonical, client } = fixture()
    canonical.title = 'Resolve upstream sync v0.0.0-unstableneutron.0'
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('candidate title')
  })

  test('rejects malformed candidate body data', async () => {
    const { payload, canonical, client } = fixture()
    canonical.body = canonical.body.replace('- Fresh: **true**', '- Fresh: **false**')
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('fresh status')
  })

  test('rejects a moved fork base', async () => {
    const { payload, client, values } = fixture()
    values.set(`/repos/${repository}/commits/main`, { sha: '8'.repeat(40) })
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('fork main moved')
  })

  test('rejects a candidate whose release tag already exists', async () => {
    const { payload, client, values } = fixture()
    values.set(`/repos/${repository}/git/ref/tags/${expectedTag}`, { ref: `refs/tags/${expectedTag}` })
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('tag already exists')
  })

  test('rejects a source workflow outside the freshness window', async () => {
    const { payload, client, values } = fixture()
    const run = values.get(`/repos/${repository}/actions/runs/12345`) as { created_at: string }
    run.created_at = '2026-08-14T00:00:00Z'
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('workflow run is not fresh')
  })
})
