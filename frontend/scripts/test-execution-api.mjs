import assert from 'node:assert/strict'
import { APIError, fetchExecutionBoard, setExecutionBoardEnabled } from '../.runtime/execution-api-test/api.js'
import { registerRuntimeTranslator } from '../.runtime/execution-api-test/i18nRuntime.js'

registerRuntimeTranslator(key => key)
const saved = {
  enabled: true, status: 'watching', goal: 'Inspect a failure',
  current: { action: 'Read failing test', purpose: 'Find cause', blocker: '', next_step: 'Fix cause', source_refs: [] },
  activity: { kind: 'tool', tool_name: 'bash', source_refs: [] },
  stages: [], updates: [], pending_events: 1, source_limited: false, provider_id: 7,
}
const requests = []
const originalFetch = globalThis.fetch
try {
  globalThis.fetch = async (url, options) => {
    requests.push({ url: String(url), options })
    return Response.json(saved)
  }
  const controller = new AbortController()
  assert.deepEqual(await fetchExecutionBoard('project/session ?#', 'pi', 'zh-CN', controller.signal), saved)
  // Merely reading saved enabled state must never trigger a paid start POST.
  assert.equal(requests.length, 1)
  assert.equal(requests[0].options.method ?? 'GET', 'GET')
  assert.equal(requests[0].options.body, undefined)
  assert.equal(requests[0].options.signal, controller.signal)
  const readURL = new URL(requests[0].url, 'http://localhost')
  assert.equal(readURL.pathname, '/api/sessions/project%2Fsession%20%3F%23/execution')
  assert.equal(readURL.searchParams.get('agent'), 'pi')
  assert.equal(readURL.searchParams.get('locale'), 'zh-CN')

  await setExecutionBoardEnabled('project/session', { agent: 'pi', locale: 'en', enabled: true, provider_id: 7 }, controller.signal)
  await setExecutionBoardEnabled('project/session', { agent: 'pi', locale: 'en', enabled: false }, controller.signal)
  assert.equal(requests.length, 3)
  const [start, pause] = requests.slice(1).map(request => request.options)
  assert.equal(start.method, 'POST')
  assert.equal(start.headers['Content-Type'], 'application/json')
  assert.equal(start.signal, controller.signal)
  assert.deepEqual(JSON.parse(start.body), { agent: 'pi', locale: 'en', enabled: true, provider_id: 7 })
  // Pause must work even if a previously saved provider has been removed.
  assert.deepEqual(JSON.parse(pause.body), { agent: 'pi', locale: 'en', enabled: false })

  globalThis.fetch = async () => Response.json({ code: 'no_provider', detail: 'Select a source' }, { status: 412 })
  await assert.rejects(
    setExecutionBoardEnabled('session', { agent: 'pi', locale: 'en', enabled: true }),
    error => error instanceof APIError && error.status === 412 && error.code === 'no_provider' && error.detail === 'Select a source',
  )
  globalThis.fetch = async () => new Response('Gateway unavailable', { status: 503 })
  await assert.rejects(fetchExecutionBoard('session', 'pi', 'en'), error => error instanceof APIError && error.status === 503 && error.code === 'execution_load_failed')
  globalThis.fetch = async () => new Response('<html>frontend fallback</html>', { headers: { 'Content-Type': 'text/html' } })
  await assert.rejects(fetchExecutionBoard('session', 'pi', 'en'), error => error instanceof APIError && error.code === 'invalid_response')
  // An aborted read is surfaced to the poller for cancellation, never retried as a POST.
  globalThis.fetch = async (_url, options) => {
    options.signal.throwIfAborted()
    throw new Error('Expected aborted signal')
  }
  controller.abort()
  await assert.rejects(fetchExecutionBoard('session', 'pi', 'en', controller.signal), error => error.name === 'AbortError')
} finally {
  globalThis.fetch = originalFetch
}
console.log('execution API safety and error contract passed')
