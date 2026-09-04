// Detects transient streaming-Markdown DOM loss and closed-fence spill without screenshots.
// usage: probe markdownwatch [seconds=30]  |  probe markdownwatch --self-test

function scanCompletedTopLevelFences(source) {
  const fences = [];
  let lineStart = 0;
  while (lineStart < source.length) {
    let lineEnd = source.indexOf('\n', lineStart);
    const hasNewline = lineEnd !== -1;
    if (!hasNewline) lineEnd = source.length;

    let cursor = lineStart;
    let indent = 0;
    while (cursor < lineEnd && source.charCodeAt(cursor) === 32 && indent < 4) {
      cursor++;
      indent++;
    }
    const marker = source[cursor];
    let runEnd = cursor;
    if ((marker === '`' || marker === '~') && indent <= 3) {
      while (runEnd < lineEnd && source[runEnd] === marker) runEnd++;
    }
    const runLength = runEnd - cursor;
    let opener = runLength >= 3;
    if (opener && marker === '`') {
      for (let index = runEnd; index < lineEnd; index++) {
        if (source[index] === '`') {
          opener = false;
          break;
        }
      }
    }
    if (!opener) {
      if (!hasNewline) break;
      lineStart = lineEnd + 1;
      continue;
    }

    const bodyStart = hasNewline ? lineEnd + 1 : lineEnd;
    let candidateStart = bodyStart;
    let closed = false;
    while (candidateStart <= source.length) {
      let candidateEnd = source.indexOf('\n', candidateStart);
      const candidateHasNewline = candidateEnd !== -1;
      if (!candidateHasNewline) candidateEnd = source.length;
      let closeCursor = candidateStart;
      let closeIndent = 0;
      while (
        closeCursor < candidateEnd &&
        source.charCodeAt(closeCursor) === 32 &&
        closeIndent < 4
      ) {
        closeCursor++;
        closeIndent++;
      }
      let closeRunEnd = closeCursor;
      while (closeRunEnd < candidateEnd && source[closeRunEnd] === marker) {
        closeRunEnd++;
      }
      let validCloser = closeIndent <= 3 && closeRunEnd - closeCursor >= runLength;
      for (let index = closeRunEnd; validCloser && index < candidateEnd; index++) {
        const code = source.charCodeAt(index);
        if (code !== 9 && code !== 13 && code !== 32) validCloser = false;
      }
      if (validCloser) {
        let bodyEnd = candidateStart;
        if (bodyEnd > bodyStart && source.charCodeAt(bodyEnd - 1) === 10) bodyEnd--;
        if (bodyEnd > bodyStart && source.charCodeAt(bodyEnd - 1) === 13) bodyEnd--;
        fences.push({ bodyStart, bodyEnd, indent, marker, runLength });
        lineStart = candidateHasNewline ? candidateEnd + 1 : source.length;
        closed = true;
        break;
      }
      if (!candidateHasNewline) break;
      candidateStart = candidateEnd + 1;
    }
    // An open fence consumes the rest of the document. A marker-looking line
    // inside it cannot open another top-level block. Keep its geometry as a
    // non-index property so callers can distinguish one legitimate volatile
    // code block from a stale extra code component after every fence closed.
    if (!closed) {
      fences.open = {
        bodyStart,
        bodyEnd: source.length,
        indent,
        marker,
        runLength,
      };
      break;
    }
  }
  return fences;
}

function expectedOpenFenceText(source, fence) {
  let body = source.slice(fence.bodyStart, fence.bodyEnd);
  const lastNewline = body.lastIndexOf('\n');
  if (lastNewline < 0) return body;
  const lastLine = body
    .slice(lastNewline + 1)
    .replace(/^[ \t]*(?:>[ \t]*)*/, '');
  if (
    lastLine.length > 0 &&
    lastLine.length < fence.runLength &&
    Array.from(lastLine).every((character) => character === fence.marker)
  ) {
    body = body.slice(0, lastNewline);
  }
  return body;
}

function runSelfTest() {
  const cases = [
    ['```ts\nconst x = 1;\n```\n\nafter', ['const x = 1;']],
    ['~~~\na\n\n~~~   \n```js\nb\n````', ['a\n', 'b']],
    ['````ts\na\n```\nb\n`````\nend', ['a\n```\nb']],
    ['```ts\nstill open', []],
    ['```ts', []],
    ['```ts\npartial closer\n`', []],
    ['```ts\npartial closer\n``', []],
    ['`` ` invalid opener\n```ts\na\n```', ['a']],
  ];
  for (const [source, expected] of cases) {
    const fences = scanCompletedTopLevelFences(source);
    const actual = fences.map(
      ({ bodyStart, bodyEnd }) => source.slice(bodyStart, bodyEnd),
    );
    if (JSON.stringify(actual) !== JSON.stringify(expected)) {
      throw new Error(`fence scanner mismatch: ${JSON.stringify({ source, expected, actual })}`);
    }
    if (fences.open) {
      const sealed = parseOpenFenceForSelfTest(source, fences.open);
      const openText = expectedOpenFenceText(source, fences.open);
      if (openText !== sealed) {
        throw new Error(`open fence mismatch: ${JSON.stringify({ source, sealed, openText })}`);
      }
    }
  }
  console.log(`markdownwatch self-test: ${cases.length} cases passed`);
}

function parseOpenFenceForSelfTest(source, fence) {
  let body = source.slice(fence.bodyStart, fence.bodyEnd);
  const partial = body.match(/\n([ \t]*)([`~]+)$/);
  if (
    partial &&
    partial[2][0] === fence.marker &&
    partial[2].length < fence.runLength
  ) {
    body = body.slice(0, partial.index);
  }
  return body;
}

if (process.argv[2] === '--self-test') {
  runSelfTest();
} else {
  const { connectPage, done, evaluate, sleep } = await import('./lib/cdp.mjs');
  const seconds = Number(process.argv[2] ?? 30);
  if (!Number.isFinite(seconds) || seconds <= 0) {
    throw new Error(`probe: markdownwatch seconds must be positive, got ${process.argv[2]}`);
  }
  const page = await connectPage();
  const runNonce = `${Date.now().toString(36)}_${process.pid}`;
  const stateKey = `__aoMarkdownWatch_${runNonce}`;
  const mismatchBinding = `__aoReportMarkdownMismatch_${runNonce}`;
  const resultBinding = `__aoReportMarkdownResult_${runNonce}`;
  let resultPage;
  let pollPage;
  let exitCode = 0;
  let newDocumentScript;
  const transportWarnings = [];
  let reportedResult;
  let resolveReportedResult;
  const reportedResultReady = new Promise((resolve) => {
    resolveReportedResult = resolve;
  });
  try {
    const scannerSource = scanCompletedTopLevelFences.toString();
    const openFenceTextSource = expectedOpenFenceText.toString();
    await page.send('Runtime.enable');
    await page.send('Runtime.addBinding', {
      name: mismatchBinding,
    });
    await page.send('Runtime.addBinding', {
      name: resultBinding,
    });
    page.on((event) => {
      if (event.method === 'Runtime.consoleAPICalled') {
        const message = (event.params?.args ?? []).map(
          (arg) => arg.value ?? arg.description ?? '',
        ).join(' ');
        if (
          message.includes('wsClient: dropped ') ||
          message.includes('wsClient: event gap on ')
        ) {
          if (transportWarnings.length < 32) transportWarnings.push(message);
        }
        return;
      }
      if (
        event.method === 'Runtime.bindingCalled' &&
        event.params?.name === mismatchBinding
      ) {
        try {
          console.log(JSON.stringify({
            markdownwatchEarlyMismatch: JSON.parse(event.params.payload),
          }, null, 2));
        } catch (error) {
          console.error('markdownwatch binding payload was invalid:', error);
        }
        return;
      }
      if (
        event.method === 'Runtime.bindingCalled' &&
        event.params?.name === resultBinding
      ) {
        try {
          reportedResult = JSON.parse(event.params.payload);
          resolveReportedResult?.();
        } catch (error) {
          console.error('markdownwatch result binding payload was invalid:', error);
        }
      }
    });
    const installSource = `(() => {
      const stateKey = ${JSON.stringify(stateKey)};
      const mismatchBinding = ${JSON.stringify(mismatchBinding)};
      if (window[stateKey]?.running) {
        return 'already armed';
      }
      const scanCompletedTopLevelFences = ${scannerSource};
      const expectedOpenFenceText = ${openFenceTextSource};
      const previous = new Map();
      const mismatchSignatures = new Map();
      const bodyIDs = new WeakMap();
      let nextBodyID = 1;
      const incrementalLexPrevious = new WeakMap();
      const incrementalLexRoots = new WeakSet();
      const documentParsePrevious = new WeakMap();
      const state = window[stateKey] = {
        running: true,
        startedAt: performance.now(),
        samples: 0,
        mutationBatches: 0,
        rowsObserved: 0,
        maxConcurrentRows: 0,
        missingDiagnostics: 0,
        canonicalAdvances: 0,
        parserAdvances: 0,
        directOnlyAdvances: 0,
        canonicalCodeUnits: 0,
        parserCodeUnits: 0,
        incrementalLexRoots: 0,
        incrementalLex: {
          calls: 0,
          inputCodeUnits: 0,
          byPath: {},
        },
        documentParseCalls: 0,
        parsePathSamples: {},
        sourceRegressions: [],
        sourceRewrites: [],
        largeSourceAdvances: [],
        largeDomDrops: [],
        fenceMismatches: [],
        fenceMismatchFrames: 0,
        reportedMismatch: false,
        maxDomDrop: 0,
        raf: 0,
        queued: false,
      };
      for (const root of document.querySelectorAll('.md-committed, .md-volatile')) {
        const calls = root.__aoStreamdownDiagnostics?.documentParseCalls;
        if (typeof calls === 'number') documentParsePrevious.set(root, calls);
      }
      const head = (value) => value.slice(0, 128);
      const tail = (value) => value.slice(Math.max(0, value.length - 96));
      const record = (target, value, cap = 32) => {
        if (target.length < cap) target.push(value);
      };
      const commonPrefixLength = (left, right) => {
        const end = Math.min(left.length, right.length);
        let index = 0;
        while (index < end && left.charCodeAt(index) === right.charCodeAt(index)) index++;
        return index;
      };
      const regionsFor = (body) => {
        const roots = Array.from(
          body.querySelectorAll('.md-committed, .md-volatile'),
        );
        return {
          count: roots.length,
          tail: roots.slice(-12).map((root) => {
            const diagnostics = root.__aoStreamdownDiagnostics;
            const content = diagnostics?.content || '';
            return {
              region: root.classList.contains('md-committed') ? 'committed' : 'volatile',
              contentLength: content.length,
              contentHead: head(content),
              contentTail: tail(content),
              parsePath: diagnostics?.lastPath ?? '<missing>',
              trailingBlock: diagnostics?.trailingBlock ?? null,
            };
          }),
        };
      };
      const codeMismatchDetail = (
        body,
        diagnostics,
        codeNode,
        index,
        expected,
        reason,
      ) => {
        const actual = codeNode?.textContent;
        const codeRoot = codeNode?.closest('[data-code-source]');
        const codeDiagnostics = codeRoot?.__aoCodeDiagnostics;
        const streamdownRoot = codeRoot?.closest('.md-committed, .md-volatile');
        const streamdownDiagnostics = streamdownRoot?.__aoStreamdownDiagnostics;
        const blocks = streamdownDiagnostics?.blocks || [];
        return {
          reason,
          index,
          expectedLength: expected?.length ?? -1,
          actualLength: actual?.length ?? -1,
          expectedTail: expected === undefined ? '<no code block>' : tail(expected),
          actualTail: actual === undefined ? '<missing>' : tail(actual),
          canonicalSourceLength: diagnostics.canonicalSource.length,
          canonicalSourceTail: tail(diagnostics.canonicalSource),
          parserIsCanonicalPrefix: diagnostics.canonicalSource.startsWith(
            diagnostics.parserSource,
          ),
          parserSourceLength: diagnostics.parserSource.length,
          parserSourceTail: tail(diagnostics.parserSource),
          region: streamdownRoot?.classList.contains('md-committed')
            ? 'committed'
            : 'volatile',
          regionSourceLength: streamdownDiagnostics?.content.length ?? -1,
          regionSourceTail: streamdownDiagnostics
            ? tail(streamdownDiagnostics.content)
            : '<missing>',
          regions: regionsFor(body),
          parsePath: streamdownDiagnostics?.lastPath ?? '<missing>',
          trailingBlock: streamdownDiagnostics?.trailingBlock ?? null,
          blockTails: Array.from(blocks).slice(-3).map((block) => ({
            length: block.length,
            tail: tail(block),
          })),
          tokenLength: codeDiagnostics?.tokenText.length ?? -1,
          tokenTail: codeDiagnostics ? tail(codeDiagnostics.tokenText) : '<missing>',
          renderedLength: codeDiagnostics?.renderedText.length ?? -1,
          renderedTail: codeDiagnostics
            ? tail(codeDiagnostics.renderedText)
            : '<missing>',
          renderedLinesLength: codeDiagnostics?.renderedLines.length ?? -1,
          spansForLength: codeDiagnostics?.spansFor.length ?? -1,
        };
      };
      const sample = (timestamp = performance.now()) => {
        state.queued = false;
        state.raf = 0;
        if (!state.running) return;
        state.samples++;
        for (const root of document.querySelectorAll('.md-committed, .md-volatile')) {
          const streamdownDiagnostics = root.__aoStreamdownDiagnostics;
          const documentCalls = streamdownDiagnostics?.documentParseCalls;
          if (typeof documentCalls === 'number') {
            const priorDocumentCalls = documentParsePrevious.get(root) ?? 0;
            state.documentParseCalls += Math.max(0, documentCalls - priorDocumentCalls);
            documentParsePrevious.set(root, documentCalls);
            const path = streamdownDiagnostics.lastPath ?? '<missing>';
            state.parsePathSamples[path] = (state.parsePathSamples[path] ?? 0) + 1;
          }
          const metrics = streamdownDiagnostics?.incrementalLexMetrics;
          if (!metrics) continue;
          if (!incrementalLexRoots.has(root)) {
            incrementalLexRoots.add(root);
            state.incrementalLexRoots++;
          }
          let priorMetrics = incrementalLexPrevious.get(root);
          if (!priorMetrics) {
            priorMetrics = { calls: 0, inputCodeUnits: 0, byPath: {} };
            incrementalLexPrevious.set(root, priorMetrics);
          }
          state.incrementalLex.calls += metrics.calls - priorMetrics.calls;
          state.incrementalLex.inputCodeUnits +=
            metrics.inputCodeUnits - priorMetrics.inputCodeUnits;
          priorMetrics.calls = metrics.calls;
          priorMetrics.inputCodeUnits = metrics.inputCodeUnits;
          for (const [path, values] of Object.entries(metrics.byPath)) {
            const priorPath = priorMetrics.byPath[path] ||= {
              calls: 0,
              inputCodeUnits: 0,
            };
            const aggregate = state.incrementalLex.byPath[path] ||= {
              calls: 0,
              inputCodeUnits: 0,
            };
            aggregate.calls += values.calls - priorPath.calls;
            aggregate.inputCodeUnits += values.inputCodeUnits - priorPath.inputCodeUnits;
            priorPath.calls = values.calls;
            priorPath.inputCodeUnits = values.inputCodeUnits;
          }
        }
        const bodies = document.querySelectorAll('[data-testid="assistant-message-body"]');
        const activeKeys = new Set();
        let activeRows = 0;
        for (const body of bodies) {
          const diagnostics = body.__aoMarkdownDiagnostics;
          if (!diagnostics) {
            state.missingDiagnostics++;
            continue;
          }
          if (!diagnostics.streaming) continue;
          activeRows++;
          state.rowsObserved++;
          const pane = body.closest('[data-pane-id]')?.getAttribute('data-pane-id') || '?';
          const thread = body.closest('[data-ui-surface="chat"]')
            ?.getAttribute('data-thread-id') || '?';
          let bodyID = bodyIDs.get(body);
          if (bodyID === undefined) {
            bodyID = nextBodyID++;
            bodyIDs.set(body, bodyID);
          }
          const key = pane + ':' + diagnostics.itemId;
          activeKeys.add(key);
          const source = diagnostics.canonicalSource;
          const parserSource = diagnostics.parserSource;
          const markdown = body.querySelector('.markdown-body');
          const domText = markdown?.textContent || '';
          const prior = previous.get(key);
          if (prior) {
            const canonicalAdvance = source.length - prior.sourceLength;
            const parserAdvance = parserSource.length - prior.parserSourceLength;
            const commonPrefix = commonPrefixLength(prior.source, source);
            if (canonicalAdvance > 0) {
              state.canonicalAdvances++;
              state.canonicalCodeUnits += canonicalAdvance;
              if (parserAdvance === 0) state.directOnlyAdvances++;
              if (canonicalAdvance >= 128) {
                record(state.largeSourceAdvances, {
                  at: Math.round(timestamp - state.startedAt), key, thread, bodyID,
                  advance: canonicalAdvance,
                  from: prior.sourceLength,
                  to: source.length,
                  commonPrefix,
                  priorSourceTail: tail(prior.source),
                  sourceTail: tail(source),
                  parserSourceLength: parserSource.length,
                  priorParserSourceLength: prior.parserSourceLength,
                });
              }
            }
            if (commonPrefix < prior.sourceLength && source.length >= prior.sourceLength) {
              const windowStart = Math.max(0, commonPrefix - 128);
              const rewrite = {
                at: Math.round(timestamp - state.startedAt), key, thread, bodyID,
                sameBody: bodyID === prior.bodyID,
                from: prior.sourceLength, to: source.length,
                commonPrefix,
                priorDifferenceWindow: prior.source.slice(
                  windowStart,
                  Math.min(prior.source.length, commonPrefix + 384),
                ),
                sourceDifferenceWindow: source.slice(
                  windowStart,
                  Math.min(source.length, commonPrefix + 384),
                ),
                priorSourceTail: tail(prior.source),
                sourceTail: tail(source),
                parserSourceLength: parserSource.length,
                priorParserSourceLength: prior.parserSourceLength,
                priorParserSourceTail: prior.parserSourceTail,
                parserSourceTail: tail(parserSource),
              };
              record(state.sourceRewrites, rewrite);
              if (!state.reportedMismatch) {
                state.reportedMismatch = true;
                window[mismatchBinding]?.(JSON.stringify({
                  ...rewrite,
                  reason: 'canonical-source-rewrite',
                }));
              }
            }
            if (parserAdvance > 0) {
              state.parserAdvances++;
              state.parserCodeUnits += parserAdvance;
            }
            if (source.length < prior.sourceLength) {
              const regression = {
                at: Math.round(timestamp - state.startedAt), key, thread, bodyID,
                sameBody: bodyID === prior.bodyID,
                from: prior.sourceLength, to: source.length,
                commonPrefix: commonPrefixLength(prior.source, source),
                priorSourceTail: tail(prior.source),
                sourceTail: tail(source),
                parserSourceLength: parserSource.length,
                priorParserSourceLength: prior.parserSourceLength,
                priorParserSourceTail: prior.parserSourceTail,
                parserSourceTail: tail(parserSource),
              };
              record(state.sourceRegressions, regression);
              if (!state.reportedMismatch) {
                state.reportedMismatch = true;
                window[mismatchBinding]?.(JSON.stringify({
                  ...regression,
                  reason: 'canonical-source-regression',
                }));
              }
            }
            const drop = prior.domLength - domText.length;
            if (drop > state.maxDomDrop) state.maxDomDrop = drop;
            if (source.length >= prior.sourceLength && drop >= 128) {
              record(state.largeDomDrops, {
                at: Math.round(timestamp - state.startedAt), key, drop,
                sourceLength: source.length,
                priorSourceLength: prior.sourceLength,
                domTail: tail(domText),
              });
            }
          }

          const scannedFences = scanCompletedTopLevelFences(source);
          const completed = scannedFences.filter(
            (fence) => fence.indent === 0,
          );
          const openFence = scannedFences.open?.indent === 0
            ? scannedFences.open
            : undefined;
          const code = markdown?.querySelectorAll('[data-code-source] code') || [];
          let mismatch = '';
          for (const root of body.querySelectorAll('.md-committed')) {
            const regionSource = root.__aoStreamdownDiagnostics?.content;
            if (typeof regionSource !== 'string') continue;
            const regionFences = scanCompletedTopLevelFences(regionSource);
            if (!regionFences.open) continue;
            mismatch = JSON.stringify({
              reason: 'open-fence-in-committed-region',
              canonicalSourceLength: source.length,
              canonicalSourceTail: tail(source),
              parserSourceLength: diagnostics.parserSource.length,
              parserSourceTail: tail(diagnostics.parserSource),
              regions: regionsFor(body),
            });
            break;
          }
          for (let index = 0; index < code.length; index++) {
            if (mismatch) break;
            if (!code.item(index)?.textContent?.includes('Visible progress marker')) continue;
            mismatch = JSON.stringify(codeMismatchDetail(
              body,
              diagnostics,
              code.item(index),
              index,
              undefined,
              'workload-marker-rendered-as-code',
            ));
            break;
          }
          for (let index = 0; index < completed.length; index++) {
            if (mismatch) break;
            const fence = completed[index];
            const expected = source.slice(fence.bodyStart, fence.bodyEnd);
            const codeNode = code.item(index);
            const actual = codeNode?.textContent;
            if (actual !== expected) {
              mismatch = JSON.stringify(codeMismatchDetail(
                body,
                diagnostics,
                codeNode,
                index,
                expected,
                'completed-fence-body-mismatch',
              ));
              break;
            }
          }
          if (!mismatch && openFence) {
            const index = completed.length;
            const expected = expectedOpenFenceText(source, openFence);
            const codeNode = code.item(index);
            const actual = codeNode?.textContent;
            if (
              actual === undefined ||
              actual.trimEnd() !== expected.trimEnd()
            ) {
              mismatch = JSON.stringify(codeMismatchDetail(
                body,
                diagnostics,
                codeNode,
                index,
                expected,
                'open-fence-body-mismatch',
              ));
            }
          }
          const expectedCodeCount = completed.length + (openFence ? 1 : 0);
          if (!mismatch && code.length > expectedCodeCount) {
            mismatch = JSON.stringify(codeMismatchDetail(
              body,
              diagnostics,
              code.item(expectedCodeCount),
              expectedCodeCount,
              undefined,
              'extra-code-block',
            ));
          }
          if (mismatch) {
            state.fenceMismatchFrames++;
            if (mismatchSignatures.get(key) !== mismatch) {
              mismatchSignatures.set(key, mismatch);
              const entry = {
                at: Math.round(timestamp - state.startedAt),
                key,
                sourceLength: source.length,
                detail: JSON.parse(mismatch),
              };
              record(state.fenceMismatches, entry);
              if (!state.reportedMismatch) {
                state.reportedMismatch = true;
                window[mismatchBinding]?.(JSON.stringify(entry));
              }
            }
          } else {
            mismatchSignatures.delete(key);
          }
          previous.set(key, {
            bodyID,
            source,
            sourceLength: source.length,
            parserSourceLength: parserSource.length,
            parserSourceTail: tail(parserSource),
            domLength: domText.length,
          });
        }
        for (const key of previous.keys()) {
          if (activeKeys.has(key)) continue;
          previous.delete(key);
          mismatchSignatures.delete(key);
        }
        state.maxConcurrentRows = Math.max(state.maxConcurrentRows, activeRows);
      };
      const schedule = () => {
        if (!state.running || state.queued) return;
        state.queued = true;
        state.raf = requestAnimationFrame(sample);
      };
      state.sample = sample;
      state.observer = new MutationObserver(() => {
        state.mutationBatches++;
        schedule();
      });
      state.observer.observe(document.documentElement, {
        subtree: true,
        childList: true,
        characterData: true,
      });
      schedule();
      return 'armed';
    })()`;
    // Bench setup reloads the frontend after resetting its isolated store. Arm
    // both the current document and every replacement document so the watcher
    // follows that reset instead of silently losing its state before streaming
    // begins. Only the final document's state matters because it owns the run.
    newDocumentScript = await page.send('Page.addScriptToEvaluateOnNewDocument', {
      source: installSource,
    });
    await evaluate(page, installSource);
    // Preserve the first mismatch outside the renderer before a safety ceiling,
    // crash, or later parser update can erase the evidence. The page-side
    // MutationObserver samples every painted mutation batch. This low-rate poll
    // only asks whether that recorder has captured a mismatch, then stops the
    // run immediately so the detailed final snapshot remains available.
    pollPage = page;
    const deadline = Date.now() + seconds * 1000;
    while (Date.now() < deadline) {
      await sleep(Math.min(250, deadline - Date.now()));
      let peek;
      try {
        peek = JSON.parse(await evaluate(pollPage, `(() => {
          const state = window[${JSON.stringify(stateKey)}];
          return JSON.stringify({
            present: Boolean(state),
            fenceMismatches: state?.fenceMismatches?.length ?? 0,
            largeDomDrops: state?.largeDomDrops?.length ?? 0,
            sourceRegressions: state?.sourceRegressions?.length ?? 0,
            sourceRewrites: state?.sourceRewrites?.length ?? 0,
          });
        })()`));
      } catch {
        if (pollPage !== page) pollPage.close();
        pollPage = await connectPage();
        await evaluate(pollPage, installSource);
        continue;
      }
      if (!peek.present) {
        // WebView2 does not consistently run Page.addScriptToEvaluateOnNewDocument
        // across the harness reset navigation. A missing state is a navigation,
        // not a clean sample. Re-arm within this 250ms poll before streaming
        // begins instead of reaching the deadline with no evidence.
        await evaluate(pollPage, installSource);
        continue;
      }
      if (
        peek.fenceMismatches > 0 ||
        peek.largeDomDrops > 0 ||
        peek.sourceRegressions > 0 ||
        peek.sourceRewrites > 0
      ) break;
    }
    resultPage = pollPage;
    // Send the detailed result over a binding and return only a tiny CDP value.
    // A long soak can accumulate enough bounded diagnostic detail that returning
    // it as Runtime.evaluate's value makes WebView2 finish the expression but
    // stall the command response. The binding event is delivered independently,
    // so the operator gets the evidence even if that response is lost.
    const reportTask = evaluate(resultPage, `(() => {
      const stateKey = ${JSON.stringify(stateKey)};
      const state = window[stateKey];
      if (!state) throw new Error('markdownwatch state disappeared');
      state.sample?.();
      state.running = false;
      state.observer?.disconnect();
      if (state.raf) cancelAnimationFrame(state.raf);
      const result = {
        seconds: Math.round((performance.now() - state.startedAt) / 100) / 10,
        samples: state.samples,
        mutationBatches: state.mutationBatches,
        rowsObserved: state.rowsObserved,
        maxConcurrentRows: state.maxConcurrentRows,
        missingDiagnostics: state.missingDiagnostics,
        canonicalAdvances: state.canonicalAdvances,
        parserAdvances: state.parserAdvances,
        directOnlyAdvances: state.directOnlyAdvances,
        canonicalCodeUnits: state.canonicalCodeUnits,
        parserCodeUnits: state.parserCodeUnits,
        incrementalLexRoots: state.incrementalLexRoots,
        incrementalLex: state.incrementalLex,
        documentParseCalls: state.documentParseCalls,
        parsePathSamples: state.parsePathSamples,
        maxDomDrop: state.maxDomDrop,
        sourceRegressions: state.sourceRegressions,
        sourceRewrites: state.sourceRewrites,
        largeSourceAdvances: state.largeSourceAdvances,
        largeDomDrops: state.largeDomDrops,
        fenceMismatchFrames: state.fenceMismatchFrames,
        fenceMismatches: state.fenceMismatches,
      };
      window[${JSON.stringify(resultBinding)}](JSON.stringify(result));
      delete window[stateKey];
      return 'reported';
    })()`).catch((error) => {
      if (reportedResult === undefined) throw error;
    });
    await Promise.race([reportTask, reportedResultReady]);
    if (reportedResult === undefined) {
      await Promise.race([reportedResultReady, sleep(2000)]);
    }
    if (reportedResult === undefined) {
      throw new Error('markdownwatch did not receive its final result binding');
    }
    const result = reportedResult;
    result.transportWarnings = transportWarnings;
    if (result.maxConcurrentRows === 0 || result.missingDiagnostics > 0) exitCode = 2;
    else if (
      result.sourceRegressions.length > 0 ||
      result.sourceRewrites.length > 0 ||
      result.largeDomDrops.length > 0 ||
      result.fenceMismatches.length > 0
    ) exitCode = 3;
  } finally {
    if (newDocumentScript?.identifier) {
      try {
        resultPage ??= await connectPage();
        await resultPage.send('Page.removeScriptToEvaluateOnNewDocument', {
          identifier: newDocumentScript.identifier,
        });
      } catch (error) {
        transportWarnings.push(`new-document script cleanup failed: ${String(error)}`);
        console.warn('markdownwatch: new-document script cleanup failed:', error);
      }
    }
    try {
      resultPage ??= await connectPage();
      await resultPage.send('Runtime.removeBinding', {
        name: mismatchBinding,
      });
      await resultPage.send('Runtime.removeBinding', {
        name: resultBinding,
      });
    } catch (error) {
      transportWarnings.push(`runtime binding cleanup failed: ${String(error)}`);
      console.warn('markdownwatch: runtime binding cleanup failed:', error);
    }
    await done(
      resultPage && resultPage !== page ? [page, resultPage] : [page],
      exitCode,
    );
  }
  if (reportedResult !== undefined) {
    reportedResult.transportWarnings = transportWarnings;
    console.log(JSON.stringify(reportedResult, null, 2));
  }
}
