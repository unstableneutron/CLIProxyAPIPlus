import type { PluginAPI, PluginThread } from '@ampcode/plugin'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { dispatchCandidate, emptyDispatchState, type DispatchState, type SpawnedThread } from './dispatch'
import { RejectedDelivery, repository, validateCandidate, verifyGitHubSignature, type GitHubClient } from './provenance'

export const description = 'Starts one fresh high-effort Orb for each strictly verified daily Upstream Sync v2 candidate PR.'

const privateDirectory = join('.amp', 'private', 'upstream-sync-v2-webhook')
const secretFile = 'github-webhook-secret'
const urlFile = 'url'
const stateKey = 'upstreamSyncV2WebhookState'

class GitHubAPI implements GitHubClient {
  constructor(private readonly token: string) {}

  async get(path: string, signal: AbortSignal, allowNotFound = false): Promise<unknown | null> {
    const response = await fetch(`https://api.github.com${path}`, {
      headers: {
        Accept: 'application/vnd.github+json',
        Authorization: `Bearer ${this.token}`,
        'X-GitHub-Api-Version': '2022-11-28',
      },
      signal,
    })
    if (allowNotFound && response.status === 404) return null
    if (!response.ok) throw new Error(`GitHub API ${path} returned ${response.status}`)
    return response.json()
  }

  async bytes(url: string, signal: AbortSignal): Promise<Uint8Array> {
    const response = await fetch(url, {
      headers: {
        Accept: 'application/vnd.github+json',
        Authorization: `Bearer ${this.token}`,
        'X-GitHub-Api-Version': '2022-11-28',
      },
      signal,
    })
    if (!response.ok) throw new Error(`GitHub artifact download returned ${response.status}`)
    return new Uint8Array(await response.arrayBuffer())
  }
}

function asDispatchState(value: unknown): DispatchState {
  if (value === undefined) return emptyDispatchState()
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('upstream-sync-v2 dispatch state is malformed')
  }
  const state = value as Partial<DispatchState>
  if (
    state.schemaVersion !== 1 ||
    !state.candidates || typeof state.candidates !== 'object' || Array.isArray(state.candidates) ||
    !state.deliveries || typeof state.deliveries !== 'object' || Array.isArray(state.deliveries)
  ) {
    throw new Error('upstream-sync-v2 dispatch state schema is unsupported')
  }
  return structuredClone(state as DispatchState)
}

function threadAdapter(thread: PluginThread): SpawnedThread {
  return {
    id: thread.id,
    append: (prompt) => thread.appendUserMessage({ type: 'user-message', content: prompt }),
    hasDelivery: async (eventID) => {
      const marker = `- Amp event: ${eventID}`
      const messages = await thread.messages({ full: true, from: 'start', limit: 20, roles: ['user'] })
      return messages.some((message) => message.content.some((block) => block.type === 'text' && block.text.includes(marker)))
    },
  }
}

let serial = Promise.resolve()

function exclusively<T>(operation: () => Promise<T>): Promise<T> {
  const next = serial.then(operation, operation)
  serial = next.then(() => undefined, () => undefined)
  return next
}

export default async function (amp: PluginAPI) {
  const rootURI = amp.system.workspaceRoot
  if (!rootURI) {
    amp.logger.log('upstream-sync-v2 webhook disabled: no workspace root')
    return
  }
  const root = amp.helpers.filePathFromURI(rootURI)
  const secretsDirectory = join(root, privateDirectory)
  let secret: string
  try {
    secret = (process.env.UPSTREAM_SYNC_WEBHOOK_SECRET ?? await readFile(join(secretsDirectory, secretFile), 'utf8')).trim()
  } catch {
    amp.logger.log(`upstream-sync-v2 webhook disabled: configure ${privateDirectory}/${secretFile}`)
    return
  }
  const token = (process.env.UPSTREAM_SYNC_GITHUB_TOKEN ?? process.env.GITHUB_TRIGGER_CI_TOKEN ?? '').trim()
  if (secret.length < 32 || !token) {
    amp.logger.log('upstream-sync-v2 webhook disabled: secret or GitHub token is missing')
    return
  }

  const github = new GitHubAPI(token)
  const registration = await amp.createWebhook({
    key: 'upstream-sync-v2-candidate',
    headers: ['x-github-event', 'x-github-delivery', 'x-hub-signature-256'],
    handler: async (event, context) => exclusively(async () => {
      if (event.headers['x-github-event'] !== 'pull_request') return
      const githubDelivery = event.headers['x-github-delivery']
      if (!githubDelivery || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(githubDelivery)) {
        amp.logger.log('upstream-sync-v2 webhook rejected: invalid GitHub delivery ID')
        return
      }
      if (!verifyGitHubSignature(event.body, event.headers['x-hub-signature-256'], secret)) {
        amp.logger.log('upstream-sync-v2 webhook rejected: invalid signature')
        return
      }

      let payload: unknown
      try {
        payload = JSON.parse(new TextDecoder().decode(event.body))
      } catch {
        amp.logger.log('upstream-sync-v2 webhook rejected: invalid JSON')
        return
      }

      try {
        const candidate = await validateCandidate(payload, event.receivedAt, github, context.signal)
        const high = amp.getBuiltinAgent('high')
        const result = await dispatchCandidate(
          {
            load: async () => {
              const configuration = await amp.configuration.get()
              return asDispatchState(configuration[stateKey])
            },
            save: (state) => amp.configuration.update({ [stateKey]: state }, 'workspace'),
          },
          {
            create: async () => threadAdapter(await high.createThread({
              parentThreadID: context.thread.id,
              executor: 'orb',
              features: [],
            })),
            get: (threadID) => threadAdapter(amp.threads.get(threadID as `T-${string}`)),
          },
          candidate,
          event.id,
        )
        amp.logger.log(`upstream-sync-v2 webhook ${result.outcome} PR #${candidate.number}${result.threadID ? ` in ${result.threadID}` : ''}`)
      } catch (error) {
        if (error instanceof RejectedDelivery) {
          amp.logger.log(`upstream-sync-v2 webhook rejected: ${error.message}`)
          return
        }
        throw error
      }
    }),
  })

  await mkdir(secretsDirectory, { recursive: true, mode: 0o700 })
  await writeFile(join(secretsDirectory, urlFile), `${registration.url}\n`, { mode: 0o600 })
  amp.logger.log(`upstream-sync-v2 webhook registered for ${repository}`)
}
