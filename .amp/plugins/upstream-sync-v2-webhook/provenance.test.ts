import { describe, expect, test } from 'bun:test'
import { createHash, createHmac } from 'node:crypto'
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
const workflowRunID = 12345
const workflowRunAttempt = 1
const artifactID = 54321
const artifactURL = `https://api.github.com/repos/${repository}/actions/artifacts/${artifactID}/zip`
const dispatchTitle = 'Upstream Sync v2 [event=workflow_dispatch mode=promote force_candidate=false repair_ref= repair_sha= repair_fingerprint= repair_pr=]'

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

## Provenance guidance
`

function crc32(value: Uint8Array): number {
  let crc = 0xffffffff
  for (const byte of value) {
    crc ^= byte
    for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ (0xedb88320 & -(crc & 1))
  }
  return (crc ^ 0xffffffff) >>> 0
}

function storedZip(files: Record<string, string>): Uint8Array {
  const local: Buffer[] = []
  const central: Buffer[] = []
  let offset = 0
  for (const [name, text] of Object.entries(files)) {
    const nameBytes = Buffer.from(name)
    const data = Buffer.from(text)
    const checksum = crc32(data)
    const localHeader = Buffer.alloc(30 + nameBytes.length)
    localHeader.writeUInt32LE(0x04034b50)
    localHeader.writeUInt16LE(20, 4)
    localHeader.writeUInt32LE(checksum, 14)
    localHeader.writeUInt32LE(data.length, 18)
    localHeader.writeUInt32LE(data.length, 22)
    localHeader.writeUInt16LE(nameBytes.length, 26)
    nameBytes.copy(localHeader, 30)
    local.push(localHeader, data)

    const centralHeader = Buffer.alloc(46 + nameBytes.length)
    centralHeader.writeUInt32LE(0x02014b50)
    centralHeader.writeUInt16LE(20, 4)
    centralHeader.writeUInt16LE(20, 6)
    centralHeader.writeUInt32LE(checksum, 16)
    centralHeader.writeUInt32LE(data.length, 20)
    centralHeader.writeUInt32LE(data.length, 24)
    centralHeader.writeUInt16LE(nameBytes.length, 28)
    centralHeader.writeUInt32LE(offset, 42)
    nameBytes.copy(centralHeader, 46)
    central.push(centralHeader)
    offset += localHeader.length + data.length
  }
  const directory = Buffer.concat(central)
  const end = Buffer.alloc(22)
  end.writeUInt32LE(0x06054b50)
  end.writeUInt16LE(central.length, 8)
  end.writeUInt16LE(central.length, 10)
  end.writeUInt32LE(directory.length, 12)
  end.writeUInt32LE(offset, 16)
  return new Uint8Array(Buffer.concat([...local, directory, end]))
}

function bodyWithConflicts(conflicts: string[]): string {
  if (conflicts.length === 0) return body
  return body.replace(
    '- Conflicts: **None**',
    ['- Conflicts:', ...conflicts.map((path) => `  - \`${path}\``)].join('\n'),
  )
}

function plannerOutput(conflicts: string[]): string {
  const values = [
    `base_fork_commit=${baseSHA}`,
    `original_tag=${originalTag}`,
    `original_head=${originalCommit}`,
    `plus_tag=${plusTag}`,
    `plus_tag_head=${plusCommit}`,
    `plus_head=${plusCommit}`,
    'plus_head_included=false',
    `models_commit=${modelsCommit}`,
    `expected_fork_tag=${expectedTag}`,
    `safe_sync_id=${syncID}`,
    `plan_fingerprint=${fingerprint}`,
    `candidate_branch=${branch}`,
    `conflicts=${conflicts.length > 0}`,
  ]
  if (conflicts.length === 0) values.push('conflict_files=')
  else values.push('conflict_files<<EOF_conflict_files', ...conflicts, 'EOF_conflict_files')
  return `${values.join('\n')}\n`
}

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
  const archiveBytes = new Map<string, Uint8Array>()
  const values = new Map<string, unknown>([
    [`/repos/${repository}/pulls/77`, canonical],
    [`/repos/${repository}/commits/main`, { sha: baseSHA }],
    ['/repos/router-for-me/CLIProxyAPI/releases/latest', { tag_name: originalTag }],
    ['/repos/kaitranntt/CLIProxyAPIPlus/releases/latest', { tag_name: plusTag }],
    ['/repos/router-for-me/CLIProxyAPI/commits/v7.2.133', { sha: originalCommit }],
    ['/repos/kaitranntt/CLIProxyAPIPlus/commits/v7.2.127-4', { sha: plusCommit }],
    ['/repos/kaitranntt/CLIProxyAPIPlus/commits/main', { sha: plusCommit }],
    ['/repos/router-for-me/models/commits/main', { sha: modelsCommit }],
    [`/repos/${repository}/actions/runs/${workflowRunID}`, {
      id: workflowRunID,
      repository: { id: repositoryID, full_name: repository },
      head_repository: { id: repositoryID, full_name: repository },
      path: '.github/workflows/upstream-sync-v2.yml',
      event: 'schedule',
      head_branch: 'main',
      head_sha: baseSHA,
      created_at: '2026-08-15T05:35:00Z',
      run_attempt: workflowRunAttempt,
      actor: owner,
      triggering_actor: owner,
      display_title: 'Upstream Sync v2',
    }],
    [`/repos/${repository}/pulls?state=open&base=main&per_page=100`, [canonical]],
  ])
  const setPlannerConflicts = (conflicts: string[]) => {
    const archive = storedZip({
      'work/plan.out': plannerOutput(conflicts),
      'work/report/candidate.md': bodyWithConflicts(conflicts),
    })
    archiveBytes.set(artifactURL, archive)
    values.set(`/repos/${repository}/actions/runs/${workflowRunID}/artifacts?per_page=100`, {
      total_count: 1,
      artifacts: [{
        id: artifactID,
        name: `upstream-sync-v2-${workflowRunID}-${workflowRunAttempt}`,
        size_in_bytes: archive.length,
        digest: `sha256:${createHash('sha256').update(archive).digest('hex')}`,
        expired: false,
        archive_download_url: artifactURL,
        workflow_run: {
          id: workflowRunID,
          repository_id: repositoryID,
          head_repository_id: repositoryID,
          head_branch: 'main',
          head_sha: baseSHA,
        },
      }],
    })
  }
  setPlannerConflicts([])
  const client: GitHubClient = {
    get: async (path, _signal, allowNotFound) => {
      if (values.has(path)) return structuredClone(values.get(path))
      if (allowNotFound) return null
      throw new Error(`missing fixture: ${path}`)
    },
    bytes: async (url) => {
      const value = archiveBytes.get(url)
      if (!value) throw new Error(`missing archive fixture: ${url}`)
      return new Uint8Array(value)
    },
  }
  return { payload, canonical, client, values, setPlannerConflicts }
}

function workflowRun(values: Map<string, unknown>): Record<string, unknown> {
  return values.get(`/repos/${repository}/actions/runs/${workflowRunID}`) as Record<string, unknown>
}

function setWorkflowDispatch(values: Map<string, unknown>): Record<string, unknown> {
  const run = workflowRun(values)
  run.event = 'workflow_dispatch'
  run.display_title = dispatchTitle
  return run
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
    const { payload, client, values } = fixture()
    const run = workflowRun(values)
    run.actor = bot
    run.triggering_actor = bot
    const candidate = await validateCandidate(payload, receivedAt, client, signal)
    expect(candidate.number).toBe(77)
    expect(candidate.planFingerprint).toBe(fingerprint)
    expect(candidate.headRef).toBe(branch)
  })

  test('accepts a populated canonical conflict list matching the planner artifact', async () => {
    const conflicts = [
      'internal/runtime/executor/codex_websockets_stream.go',
      'sdk/cliproxy/auth/conductor_stream.go',
    ]
    const { payload, canonical, client, setPlannerConflicts } = fixture()
    canonical.body = bodyWithConflicts(conflicts)
    setPlannerConflicts(conflicts)
    expect((await validateCandidate(payload, receivedAt, client, signal)).planFingerprint).toBe(fingerprint)
  })

  test('rejects a malformed populated conflict list', async () => {
    const { payload, canonical, client } = fixture()
    canonical.body = body.replace('- Conflicts: **None**', '- Conflicts:\n  - unquoted/path.go')
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('conflict list is malformed')
  })

  test('rejects conflict entries hidden under a no-conflict status', async () => {
    const { payload, canonical, client } = fixture()
    canonical.body = body.replace(
      '- Conflicts: **None**',
      '- Conflicts: **None**\n  - `attacker/spoofed.go`',
    )
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('no-conflict section has entries')
  })

  test('rejects a spoofed conflict absent from the planner artifact', async () => {
    const { payload, canonical, client } = fixture()
    canonical.body = bodyWithConflicts(['attacker/spoofed.go'])
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('differs from planner artifact')
  })

  test('rejects a conflict list that omits a planner conflict', async () => {
    const bodyConflicts = ['internal/runtime/executor/codex_websockets_stream.go']
    const plannerConflicts = [...bodyConflicts, 'sdk/cliproxy/auth/conductor_stream.go']
    const { payload, canonical, client, setPlannerConflicts } = fixture()
    canonical.body = bodyWithConflicts(bodyConflicts)
    setPlannerConflicts(plannerConflicts)
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('differs from planner artifact')
  })

  test('rejects duplicate conflict paths before consulting the artifact', async () => {
    const { payload, canonical, client } = fixture()
    canonical.body = bodyWithConflicts(['same/path.go', 'same/path.go'])
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('conflict list is duplicated')
  })

  test('rejects a fingerprint that does not match immutable inputs', async () => {
    const { payload, client, values } = fixture()
    const canonical = values.get(`/repos/${repository}/pulls/77`) as { body: string }
    canonical.body = canonical.body.replace(`Plan fingerprint: \`${fingerprint}\``, `Plan fingerprint: \`${'f'.repeat(40)}\``)
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('candidate branch does not match fingerprint')
  })

  test('accepts an exact owner-triggered promote workflow dispatch', async () => {
    const { payload, client, values } = fixture()
    setWorkflowDispatch(values)
    expect((await validateCandidate(payload, receivedAt, client, signal)).workflowRunID).toBe(workflowRunID)
  })

  test('rejects a workflow dispatch whose actor identity is spoofed', async () => {
    const { payload, client, values } = fixture()
    const run = setWorkflowDispatch(values)
    run.actor = { ...owner, id: repositoryOwnerID + 1 }
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('workflow dispatch actor identity')
  })

  test('rejects a workflow dispatch whose triggering actor identity is spoofed', async () => {
    const { payload, client, values } = fixture()
    const run = setWorkflowDispatch(values)
    run.triggering_actor = bot
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('workflow dispatch triggering actor identity')
  })

  for (const [name, title] of [
    ['shadow mode', dispatchTitle.replace('mode=promote', 'mode=shadow')],
    ['forced candidate', dispatchTitle.replace('force_candidate=false', 'force_candidate=true')],
    ['repair ref', dispatchTitle.replace('repair_ref=', 'repair_ref=repair/branch')],
    ['repair SHA', dispatchTitle.replace('repair_sha=', `repair_sha=${'a'.repeat(40)}`)],
    ['repair fingerprint', dispatchTitle.replace('repair_fingerprint=', `repair_fingerprint=${'b'.repeat(40)}`)],
    ['repair PR', dispatchTitle.replace('repair_pr=', 'repair_pr=56')],
  ]) {
    test(`rejects workflow dispatch input spoofing via ${name}`, async () => {
      const { payload, client, values } = fixture()
      const run = setWorkflowDispatch(values)
      run.display_title = title
      await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('workflow dispatch inputs')
    })
  }

  test('rejects workflow run repository spoofing', async () => {
    const { payload, client, values } = fixture()
    workflowRun(values).head_repository = { id: repositoryID + 1, full_name: repository }
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('workflow run provenance')
  })

  test('rejects workflow path spoofing', async () => {
    const { payload, client, values } = fixture()
    workflowRun(values).path = '.github/workflows/attacker.yml'
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('workflow run provenance')
  })

  test('rejects workflow ref spoofing', async () => {
    const { payload, client, values } = fixture()
    workflowRun(values).head_branch = 'attacker/ref'
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('workflow run provenance')
  })

  test('rejects workflow SHA spoofing', async () => {
    const { payload, client, values } = fixture()
    workflowRun(values).head_sha = '9'.repeat(40)
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('workflow run provenance')
  })

  test('rejects unsupported workflow events', async () => {
    const { payload, client, values } = fixture()
    workflowRun(values).event = 'repository_dispatch'
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('workflow run event is not permitted')
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

  test('rejects fork main moving during pre-claim provenance checks', async () => {
    const { payload, client } = fixture()
    let mainReads = 0
    const movingClient: GitHubClient = {
      ...client,
      get: (path, requestSignal, allowNotFound) => {
        if (path === `/repos/${repository}/commits/main` && ++mainReads === 2) {
          return Promise.resolve({ sha: '8'.repeat(40) })
        }
        return client.get(path, requestSignal, allowNotFound)
      },
    }
    await expect(validateCandidate(payload, receivedAt, movingClient, signal)).rejects.toThrow('fork main moved')
  })

  test('rejects a candidate whose release tag already exists', async () => {
    const { payload, client, values } = fixture()
    values.set(`/repos/${repository}/git/ref/tags/${expectedTag}`, { ref: `refs/tags/${expectedTag}` })
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('tag already exists')
  })

  test('rejects a source workflow outside the freshness window', async () => {
    const { payload, client, values } = fixture()
    const run = values.get(`/repos/${repository}/actions/runs/${workflowRunID}`) as { created_at: string }
    run.created_at = '2026-08-14T00:00:00Z'
    await expect(validateCandidate(payload, receivedAt, client, signal)).rejects.toThrow('workflow run is not fresh')
  })
})
