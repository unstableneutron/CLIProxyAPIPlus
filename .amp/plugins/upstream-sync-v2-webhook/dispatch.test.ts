import { describe, expect, test } from 'bun:test'
import {
  dispatchCandidate,
  emptyDispatchState,
  type DispatchState,
  type DispatchStore,
  type SpawnedThread,
  type ThreadSpawner,
} from './dispatch'
import type { Candidate } from './provenance'

const candidate: Candidate = {
  repository: 'unstableneutron/CLIProxyAPIPlus',
  repositoryID: 1247056725,
  number: 77,
  url: 'https://github.com/unstableneutron/CLIProxyAPIPlus/pull/77',
  action: 'opened',
  headRef: 'upstream-sync/test-aaaaaaaaaaaa',
  headSHA: '1'.repeat(40),
  baseSHA: '2'.repeat(40),
  syncID: 'original-v1_plus-v1',
  planFingerprint: 'a'.repeat(40),
  expectedTag: 'v1-unstableneutron.0',
  originalTag: 'v1',
  originalCommit: '3'.repeat(40),
  plusTag: 'v1-0',
  plusTagCommit: '4'.repeat(40),
  plusHeadCommit: '4'.repeat(40),
  plusHeadIncluded: false,
  modelsCommit: '5'.repeat(40),
  workflowRunID: 123,
  supersededPRs: [],
}

function memoryStore(): DispatchStore & { state: DispatchState } {
  return {
    state: emptyDispatchState(),
    async load() { return structuredClone(this.state) },
    async save(state) { this.state = structuredClone(state) },
  }
}

describe('candidate dispatch', () => {
  test('creates and prompts one thread across duplicate deliveries', async () => {
    const store = memoryStore()
    let creates = 0
    let appends = 0
    const spawner: ThreadSpawner = {
      create: async () => {
        creates++
        return { id: 'T-test', append: async () => { appends++ }, hasDelivery: async () => false }
      },
      get: () => { throw new Error('unexpected lookup') },
    }

    expect((await dispatchCandidate(store, spawner, candidate, 'delivery-1')).outcome).toBe('dispatched')
    expect((await dispatchCandidate(store, spawner, candidate, 'delivery-1')).outcome).toBe('duplicate')
    expect((await dispatchCandidate(store, spawner, candidate, 'delivery-2')).outcome).toBe('duplicate')
    expect(creates).toBe(1)
    expect(appends).toBe(1)
  })

  test('fails closed when thread creation is uncertain', async () => {
    const store = memoryStore()
    let creates = 0
    const spawner: ThreadSpawner = {
      create: async () => {
        creates++
        throw new Error('connection lost')
      },
      get: () => { throw new Error('unexpected lookup') },
    }

    expect((await dispatchCandidate(store, spawner, candidate, 'delivery-1')).outcome).toBe('blocked')
    expect((await dispatchCandidate(store, spawner, candidate, 'delivery-1')).outcome).toBe('blocked')
    expect(creates).toBe(1)
  })

  test('reuses the recorded thread when appending is retried', async () => {
    const store = memoryStore()
    let creates = 0
    let appends = 0
    const thread: SpawnedThread = {
      id: 'T-test',
      hasDelivery: async () => false,
      append: async () => {
        appends++
        if (appends === 1) throw new Error('temporary append failure')
      },
    }
    const spawner: ThreadSpawner = {
      create: async () => {
        creates++
        return thread
      },
      get: (threadID) => {
        expect(threadID).toBe('T-test')
        return thread
      },
    }

    await expect(dispatchCandidate(store, spawner, candidate, 'delivery-1')).rejects.toThrow('temporary append failure')
    expect((await dispatchCandidate(store, spawner, candidate, 'delivery-1')).outcome).toBe('dispatched')
    expect(creates).toBe(1)
    expect(appends).toBe(2)
    expect(Object.values(store.state.candidates)[0].threadID).toBe('T-test')
  })

  test('does not append twice if the final state write was interrupted', async () => {
    const store = memoryStore()
    const thread: SpawnedThread = {
      id: 'T-test',
      append: async () => { throw new Error('unexpected duplicate append') },
      hasDelivery: async () => true,
    }
    const key = `${candidate.repositoryID}:${candidate.number}:${candidate.planFingerprint}:${candidate.headSHA}`
    store.state.candidates[key] = {
      candidateKey: key,
      eventID: 'delivery-1',
      status: 'thread-created',
      updatedAt: '2026-08-15T00:00:00Z',
      threadID: thread.id,
    }
    store.state.deliveries['delivery-1'] = key
    const spawner: ThreadSpawner = {
      create: async () => { throw new Error('unexpected create') },
      get: () => thread,
    }

    expect((await dispatchCandidate(store, spawner, candidate, 'delivery-1')).outcome).toBe('dispatched')
  })
})
