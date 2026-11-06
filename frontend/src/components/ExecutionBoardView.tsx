import { useCallback, useEffect, useRef, useState } from 'react'
import { APIError, fetchExecutionBoard, fetchLLMProviders, setExecutionBoardEnabled, type ExecutionBoard, type ExecutionRef, type LLMProvider } from '../api'
import { formatDate, useI18n } from '../i18n'
import AISettingsModal from './AISettingsModal'

interface Props {
  sessionId: string
  agentType: string
  active: boolean
  onJumpToSource: (source: ExecutionRef) => void
}

const POLL_MS = 2000

// Keep polling serial and separate from the explicit enable/pause mutation.
// Hiding the board cancels reads, but never pauses the saved background service.
function useBoard(sessionId: string, agent: string, active: boolean, locale: 'en' | 'zh-CN') {
  const [board, setBoard] = useState<ExecutionBoard | null>(null)
  const [readError, setReadError] = useState(false)
  const [changeError, setChangeError] = useState<string | null>(null)
  const [changing, setChanging] = useState(false)
  const [revision, setRevision] = useState(0)
  const readController = useRef<AbortController | null>(null)
  const changeController = useRef<AbortController | null>(null)
  const generation = useRef(0)
  const mutationPending = useRef(false)

  useEffect(() => () => {
    generation.current++
    readController.current?.abort()
    changeController.current?.abort()
  }, [sessionId, agent, locale])

  useEffect(() => {
    if (!active) return
    let disposed = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const poll = async () => {
      if (disposed) return
      if (mutationPending.current || document.hidden) {
        timer = setTimeout(poll, POLL_MS)
        return
      }
      const controller = new AbortController()
      readController.current = controller
      const ticket = generation.current
      try {
        const result = await fetchExecutionBoard(sessionId, agent, locale, controller.signal)
        if (!disposed && !controller.signal.aborted && ticket === generation.current) {
          setBoard(result)
          setReadError(false)
        }
      } catch {
        if (!disposed && !controller.signal.aborted && ticket === generation.current) setReadError(true)
      } finally {
        if (!disposed) timer = setTimeout(poll, POLL_MS)
      }
    }
    void poll()
    return () => {
      disposed = true
      clearTimeout(timer)
      readController.current?.abort()
    }
  }, [sessionId, agent, active, locale, revision])

  const changeEnabled = useCallback(async (enabled: boolean, providerId?: number) => {
    if (mutationPending.current) return
    mutationPending.current = true
    generation.current++
    readController.current?.abort()
    const controller = new AbortController()
    changeController.current = controller
    setChanging(true)
    setChangeError(null)
    try {
      const result = await setExecutionBoardEnabled(sessionId, { agent, locale, enabled, provider_id: providerId }, controller.signal)
      if (!controller.signal.aborted) {
        setBoard(result)
        setReadError(false)
      }
    } catch (error) {
      if (!controller.signal.aborted) setChangeError(error instanceof APIError && error.code === 'no_provider' ? 'no_provider' : 'execution_update_failed')
    } finally {
      mutationPending.current = false
      if (!controller.signal.aborted) setChanging(false)
    }
  }, [sessionId, agent, locale])

  return { board, readError, changeError, changing, changeEnabled, retry: () => setRevision(value => value + 1) }
}

export default function ExecutionBoardView({ sessionId, agentType, active, onJumpToSource }: Props) {
  const { t, locale } = useI18n()
  const { board, readError, changeError, changing, changeEnabled, retry } = useBoard(sessionId, agentType, active, locale)
  const [providers, setProviders] = useState<LLMProvider[]>([])
  const [providersState, setProvidersState] = useState<'loading' | 'ready' | 'error'>('loading')
  const [providerRevision, setProviderRevision] = useState(0)
  const [selectedProvider, setSelectedProvider] = useState<number | undefined>()
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [selectedStage, setSelectedStage] = useState<string | null>(null)
  const [now, setNow] = useState(Date.now)

  useEffect(() => {
    if (!active) return
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [active])

  useEffect(() => {
    if (!active) return
    let controller: AbortController | null = null
    const load = () => {
      controller?.abort()
      controller = new AbortController()
      const signal = controller.signal
      setProvidersState('loading')
      void fetchLLMProviders(signal).then(result => {
        if (signal.aborted) return
        setProviders(result.providers ?? [])
        setProvidersState('ready')
      }).catch(() => { if (!signal.aborted) setProvidersState('error') })
    }
    load()
    window.addEventListener('si-ai-providers-changed', load)
    return () => {
      controller?.abort()
      window.removeEventListener('si-ai-providers-changed', load)
    }
  }, [active, providerRevision])

  const duration = (timestamp?: string) => {
    const parsed = timestamp ? Date.parse(timestamp) : NaN
    if (!Number.isFinite(parsed)) return t('execution.timeUnknown')
    const seconds = Math.max(0, Math.floor((now - parsed) / 1000))
    if (seconds < 60) return t('execution.seconds', { count: seconds })
    if (seconds < 3600) return t('execution.minutesSeconds', { minutes: Math.floor(seconds / 60), seconds: seconds % 60 })
    return t('execution.hoursMinutes', { hours: Math.floor(seconds / 3600), minutes: Math.floor(seconds % 3600 / 60) })
  }
  const providerId = providers.find(provider => provider.id === selectedProvider)?.id
    ?? providers.find(provider => provider.id === board?.provider_id)?.id
    ?? providers.find(provider => provider.is_default)?.id
    ?? providers[0]?.id
  const providerExists = providers.some(provider => provider.id === providerId)
  const enabled = board?.enabled ?? false
  const stage = board?.stages.find(item => item.id === selectedStage) ?? board?.stages.find(item => item.status === 'active') ?? board?.stages[0]
  const pending = board?.pending_events ?? 0
  const stale = pending > 0 && (!board?.updated_at || now - Date.parse(board.updated_at) > 60_000)
  const errorKey = changeError === 'no_provider' || board?.error_code === 'no_provider' ? 'execution.noProvider' : 'execution.changeFailed'

  const refs = (sources: ExecutionRef[]) => sources.length > 0 && (
    <div className="execution-refs">
      {sources.map((source, index) => (
        <button key={`${source.event_id}-${source.turn_index}-${source.tool_call_id ?? ''}-${index}`} type="button"
          className="execution-source" data-testid="execution-source-ref"
          title={t('execution.sourceTitle', { turn: source.turn_index + 1 })}
          onClick={() => onJumpToSource(source)}>
          {t(source.tool_call_id ? 'execution.toolSource' : 'execution.turnSource', { turn: source.turn_index + 1 })}
          <span aria-hidden="true"> ↗</span>
        </button>
      ))}
    </div>
  )

  return (
    <section className="execution-board" hidden={!active} aria-label={t('execution.title')} data-testid="execution-board">
      <header className="execution-toolbar">
        <div className="execution-heading">
          <span className="execution-eyebrow">{t('execution.singleSession')}</span>
          <h2>{t('execution.title')}</h2>
        </div>
        <div className="execution-controls">
          <label className="execution-provider-label">
            <span>{t('execution.sourceModel')}</span>
            <select data-testid="execution-provider" value={providerExists ? providerId : ''}
              disabled={enabled || changing || providersState !== 'ready'}
              title={t(enabled ? 'execution.pauseToChangeModel' : 'execution.sourceModelHint')}
              onChange={event => setSelectedProvider(Number(event.target.value))}>
              {!providerExists && <option value="">{t(providersState === 'loading' ? 'common.loading' : 'execution.chooseModel')}</option>}
              {providers.map(provider => <option key={provider.id} value={provider.id}>{provider.name} · {provider.model_label || provider.model_id}</option>)}
            </select>
          </label>
          <button type="button" className="execution-button" onClick={() => setSettingsOpen(true)} title={t('execution.configureHint')}
            data-testid="execution-configure">{t('execution.configure')}</button>
          <button type="button" className={`execution-button ${enabled ? '' : 'execution-button-primary'}`}
            data-testid="execution-toggle" aria-pressed={enabled}
            disabled={changing || !board || (!enabled && (!providerExists || providersState !== 'ready'))}
            title={t(enabled ? 'execution.pauseHint' : 'execution.startHint')}
            onClick={() => void changeEnabled(!enabled, providerId)}>
            {t(changing ? 'execution.saving' : enabled ? 'execution.pause' : 'execution.start')}
          </button>
        </div>
      </header>
      <div className="execution-notices">
        {readError && <div className="execution-notice execution-notice-error" role="alert" data-testid="execution-read-error">
          <span>{t('execution.loadFailed')}</span><button type="button" onClick={retry}>{t('common.retry')}</button>
        </div>}
        {(changeError || board?.error_code || board?.status === 'error') && <div className="execution-notice execution-notice-error" role="alert" data-testid="execution-service-error">{t(errorKey)}</div>}
        {providersState === 'error' && <div className="execution-notice execution-notice-error" role="alert">
          <span>{t('execution.providersFailed')}</span><button type="button" onClick={() => setProviderRevision(value => value + 1)}>{t('common.retry')}</button>
        </div>}
        {providersState === 'ready' && providers.length === 0 && <div className="execution-notice" data-testid="execution-no-provider">{t('execution.noProvider')}</div>}
      </div>
      {!board ? (
        <div className="execution-empty" role="status" data-testid="execution-loading">
          <span className="execution-empty-symbol" aria-hidden="true">◎</span>
          <h3>{t(readError ? 'execution.unavailable' : 'execution.loading')}</h3>
          <p>{t('execution.loadingHint')}</p>
        </div>
      ) : (
        <>
          <div className="execution-statusline">
            <span className={`execution-status execution-status-${board.status}`} data-testid="execution-status" role="status">
              <i aria-hidden="true" />{t(`execution.status.${board.status}`)}
            </span>
            <span>{t(enabled ? 'execution.enabledHint' : 'execution.disabledHint')}</span>
          </div>
          <div className="execution-content">
            <div className="execution-main">
              <section className="execution-current execution-card" data-testid="execution-current">
                <div className="execution-card-heading">
                  <h3>{t('execution.current')}</h3>
                  <span className="execution-ai-label">{t('execution.aiExplanation')}</span>
                </div>
                <p className="execution-goal">{t('execution.goal')}<span>{board.goal || t('execution.goalEmpty')}</span></p>
                <h4 className="execution-action">{board.current.action || t(enabled ? 'execution.waitingAction' : 'execution.readyAction')}</h4>
                <p className="execution-purpose">{board.current.purpose || t(enabled ? 'execution.waitingPurpose' : 'execution.readyPurpose')}</p>
                <dl className="execution-next">
                  <div className={board.current.blocker ? 'execution-blocked' : ''}>
                    <dt>{t('execution.blocker')}</dt><dd>{board.current.blocker || t('execution.noBlocker')}</dd>
                  </div>
                  <div><dt>{t('execution.nextStep')}</dt><dd>{board.current.next_step || t('execution.noNextStep')}</dd></div>
                </dl>
                {refs(board.current.source_refs)}
                <div className={`execution-freshness ${stale ? 'execution-stale' : ''}`} data-testid="execution-freshness" data-stale={stale}>
                  <span>{board.updated_at ? t('execution.updatedAgo', { age: duration(board.updated_at) }) : t('execution.notUpdated')}</span>
                  <span>{pending > 0 ? t(stale ? 'execution.stalePending' : 'execution.pending', { count: pending }) : t('execution.noPending')}</span>
                </div>
              </section>
              <section className="execution-card execution-route" data-testid="execution-route">
                <div className="execution-card-heading"><h3>{t('execution.route')}</h3><span>{t('execution.routeHint')}</span></div>
                {board.stages.length === 0 ? <p className="execution-placeholder">{t('execution.routeEmpty')}</p> : (
                  <>
                    <div className="execution-stages">
                      {board.stages.map((item, index) => <button key={item.id} type="button"
                        className={`execution-stage execution-stage-${item.status}`} data-testid="execution-stage"
                        aria-pressed={stage?.id === item.id} aria-current={item.status === 'active' ? 'step' : undefined}
                        onClick={() => setSelectedStage(item.id)} title={t('execution.inspectStage', { title: item.title })}>
                        <span className="execution-stage-number" aria-hidden="true">{item.status === 'completed' ? '✓' : String(index + 1).padStart(2, '0')}</span>
                        <strong>{item.title}</strong><span>{t(`execution.stage.${item.status}`)}</span>
                      </button>)}
                    </div>
                    {stage && <div className="execution-stage-detail" data-testid="execution-stage-detail"><p>{stage.summary || t('execution.stageEmpty')}</p>{refs(stage.source_refs)}</div>}
                  </>
                )}
              </section>
            </div>
            <aside className="execution-side">
              <section className="execution-card execution-activity" data-testid="execution-activity">
                <div className="execution-card-heading"><h3>{t('execution.activity')}</h3><span className="execution-observed-label">{t('execution.observed')}</span></div>
                <div className={`execution-activity-kind execution-activity-${board.activity.kind}`}>
                  <i aria-hidden="true" /><strong>{board.activity.kind === 'tool' ? board.activity.tool_name || t('execution.activity.tool') : t(`execution.activity.${board.activity.kind}`)}</strong>
                </div>
                <div className="execution-elapsed" data-testid="execution-elapsed">{t('execution.elapsed', { time: duration(board.activity.started_at) })}</div>
                {refs(board.activity.source_refs)}
                <p className="execution-observed-at">{board.observed_at ? t('execution.observedAgo', { age: duration(board.observed_at) }) : t('execution.notObserved')}</p>
              </section>
              <section className="execution-card execution-timeline" data-testid="execution-timeline">
                <div className="execution-card-heading"><h3>{t('execution.progress')}</h3><span>{t('execution.aiExplanation')}</span></div>
                {board.updates.length === 0 ? <p className="execution-placeholder">{t('execution.progressEmpty')}</p> : (
                  <ol>{[...board.updates].sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at)).map(update => (
                    <li key={update.id} data-testid="execution-update">
                      <time dateTime={update.created_at} title={formatDate(locale, update.created_at, { dateStyle: 'medium', timeStyle: 'medium' })}>
                        {formatDate(locale, update.created_at, { hour: '2-digit', minute: '2-digit', second: '2-digit' })}
                      </time>
                      <p>{update.text}</p>{refs(update.source_refs)}
                    </li>
                  ))}</ol>
                )}
              </section>
            </aside>
          </div>
          <footer className="execution-footer">
            <p>{t(board.source_limited ? 'execution.sourceLimited' : 'execution.sourceHint')}</p>
            <p>{t('execution.languageHint')}</p>
          </footer>
        </>
      )}
      {settingsOpen && <AISettingsModal onClose={() => { setSettingsOpen(false); setProviderRevision(value => value + 1) }} />}
    </section>
  )
}
