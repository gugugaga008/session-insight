# Single-session execution board

The execution board explains one session as its recorded output grows. Open a
session, choose **Execution board**, select a configured model source, and choose
**Start**. **Pause** stops explanation requests; it does not stop
the monitored agent. Model sources use the existing AI settings dialog.

The board combines two kinds of information:

- Observed activity comes from normalized session events and updates without a
  model request. An unmatched tool invocation is shown as an open recorded tool
  call; this is not proof that its operating-system process is alive.
- Current action, purpose, blockers, declared next steps, stages, and progress
  entries are labeled AI interpretations. Source buttons return to the related
  turn or tool call in the existing transcript.

The board can only see data exposed by an adapter's readable source. An agent
that writes completed messages to disk does not provide token-by-token visibility
through file monitoring. Missing output and idle activity do not imply success
or session completion.

## Runtime behavior

`internal/execution` owns window selection, output validation, and the background
monitor. It reads the existing normalized events; it does not introduce another
transcript parser or modify agent session files.

- Enabled monitors observe source changes every second. The visible frontend
  polls the board every two seconds and updates elapsed-time labels each second.
- New data normally triggers interpretation after about ten seconds. Errors and
  sufficiently large windows can trigger earlier, subject to a three-second
  cooldown. No new data means no model request.
- Only one generation runs for a given agent/session at a time, including when
  multiple tabs view that session. New events arriving during generation remain
  pending for the next window.
- Input windows, tool context, output size, stage count, and progress history are
  bounded. API model requests specify a 6,000-token output limit, allowing room
  for providers that count reasoning toward that limit. Model citations omit
  long tool-call IDs; the server restores exact links from source events.
  Opening a long session starts from a recent window and displays the
  limited-source notice. Later deltas are consumed in order.
- The model receives source data separately from instructions when the provider
  supports a system role. The existing insight redactor scrubs outgoing text.
  Citation validation checks source-reference existence; it cannot prove that a
  model's interpretation is semantically correct.
- The successful cursor advances only after generation, validation, and
  persistence succeed. Errors retain the previous explanation and retry with
  backoff. Pause cancels an in-flight request and fences late responses.
- Source-prefix fingerprints detect rewrites and truncation. Volatile progress
  markers are excluded from the summary cursor. Rewritten source invalidates
  interpretations derived from its old prefix.

The board and its verified cursor are stored in the application's SQLite data
directory. Enabled monitors resume on application restart. Paused monitors stay
paused. Changing the explanation language requires pausing and starting again
in the chosen UI language; a read request never changes the monitor language.

## API

All routes require the exact adapter name and session ID. Write routes use the
same origin protection as other application mutations.

```text
GET  /api/sessions/{id}/execution?agent=pi&locale=en
POST /api/sessions/{id}/execution
POST /api/sessions/{id}/execution/refresh
```

Enable or pause with:

```json
{"agent":"pi","locale":"en","enabled":true,"provider_id":1}
```

A GET for a never-enabled board does not authorize model requests. Enabling
without an available provider returns HTTP 412 with `no_provider`. Credentials
remain in the existing model-source store and do not appear in board responses.

## Validation

```bash
go test -tags sqlite_fts5 ./internal/execution ./internal/db ./internal/server
npm --prefix frontend run test:execution-api
npm --prefix frontend run test:i18n
npm --prefix frontend run test:i18n-source
npm --prefix frontend run lint
npm --prefix frontend run build
```

For browser validation, start the complete app with `./run.sh all` and use its
post-bind `Ready:` URL. Exercise both English and Simplified Chinese: missing
model configuration, start/pause, a tool call and interpretation update within
the same turn, stage details, source navigation, failed generation with retained
explanation, reload persistence, and resume. Check desktop geometry and browser
errors as well as visible labels.
