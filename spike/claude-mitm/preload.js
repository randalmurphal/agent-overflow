const fs = require("node:fs");
const childProcess = require("node:child_process");

const logPath = process.env.AO_MITM_LOG || "/tmp/ao-claude-mitm.jsonl";
const maxTextLength = Number(process.env.AO_MITM_MAX_TEXT || "4096");

function now() {
  return new Date().toISOString();
}

function stringifyChunk(value) {
  if (value == null) return "";
  if (typeof value === "string") return value;
  if (Buffer.isBuffer(value)) return value.toString("utf8");
  if (value instanceof Uint8Array) return Buffer.from(value).toString("utf8");
  try {
    return String(value);
  } catch {
    return "<unstringifiable>";
  }
}

function trimText(text) {
  const sanitizedText = sanitizeText(text);
  if (sanitizedText.length <= maxTextLength) return sanitizedText;
  return `${sanitizedText.slice(0, maxTextLength)}...<truncated ${sanitizedText.length - maxTextLength} chars>`;
}

function sanitizeText(text) {
  return String(text)
    .replace(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+/g, "<email>")
    .replace(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi, "<uuid>")
    .replace(/[0-9a-f]{64}/gi, "<hex64>")
    .replace(/"api[_-]?key"\s*:\s*"[^"]+"/gi, '"api_key":"<redacted>"')
    .replace(/"authorization"\s*:\s*"[^"]+"/gi, '"authorization":"<redacted>"')
    .replace(/"cookie"\s*:\s*"[^"]+"/gi, '"cookie":"<redacted>"')
    .replace(/sk-ant-[A-Za-z0-9_-]+/g, "sk-ant-<redacted>");
}

function redactHeaderValue(name, value) {
  const lowerName = String(name).toLowerCase();
  if (
    lowerName.includes("authorization") ||
    lowerName.includes("cookie") ||
    lowerName.includes("token") ||
    lowerName.includes("key") ||
    lowerName.includes("secret")
  ) {
    return "<redacted>";
  }
  return value;
}

function headersToObject(headers) {
  if (!headers) return undefined;
  const output = {};

  try {
    if (typeof headers.forEach === "function") {
      headers.forEach((value, name) => {
        output[name] = redactHeaderValue(name, value);
      });
      return output;
    }

    if (Array.isArray(headers)) {
      for (const [name, value] of headers) {
        output[name] = redactHeaderValue(name, value);
      }
      return output;
    }

    for (const [name, value] of Object.entries(headers)) {
      output[name] = redactHeaderValue(name, value);
    }
    return output;
  } catch (error) {
    return { error: `failed to read headers: ${error.message}` };
  }
}

function redactUrl(value) {
  try {
    const url = new URL(String(value));
    for (const key of [...url.searchParams.keys()]) {
      if (/token|key|secret|auth|code/i.test(key)) {
        url.searchParams.set(key, "<redacted>");
      }
    }
    return url.toString();
  } catch {
    return String(value);
  }
}

function summarizeBody(body) {
  if (body == null) return undefined;
  if (typeof body === "string") return { type: "string", text: trimText(body) };
  if (Buffer.isBuffer(body)) return { type: "buffer", text: trimText(body.toString("utf8")) };
  if (body instanceof Uint8Array) return { type: "uint8array", text: trimText(Buffer.from(body).toString("utf8")) };
  if (body instanceof URLSearchParams) return { type: "urlsearchparams", text: trimText(body.toString()) };
  if (typeof body === "object") return { type: body.constructor?.name || "object" };
  return { type: typeof body, text: trimText(String(body)) };
}

function replaceBodyText(body) {
  const replaceFrom = process.env.AO_MITM_REPLACE_FROM;
  const replaceTo = process.env.AO_MITM_REPLACE_TO;
  if (!replaceFrom || replaceTo == null || typeof body !== "string") return body;
  return body.split(replaceFrom).join(replaceTo);
}

function summarizeResponseBody(response, requestInfo) {
  if (typeof response.clone !== "function") return;

  try {
    response
      .clone()
      .text()
      .then((text) => {
        emit({
          kind: "fetch_response_body",
          url: requestInfo.url,
          status: response.status,
          bytes: Buffer.byteLength(text),
          text: trimText(text),
        });
      })
      .catch((error) => {
        emit({
          kind: "fetch_response_body_error",
          url: requestInfo.url,
          status: response.status,
          error: error.message,
        });
      });
  } catch (error) {
    emit({
      kind: "fetch_response_body_error",
      url: requestInfo.url,
      status: response.status,
      error: error.message,
    });
  }
}

function emit(event) {
  const line = JSON.stringify({ ts: now(), pid: process.pid, ...event }) + "\n";
  try {
    fs.appendFileSync(logPath, line, "utf8");
  } catch {
    // Avoid interfering with Claude if the diagnostic log cannot be written.
  }
}

function installWriteHook(stream, name) {
  const originalWrite = stream.write.bind(stream);
  stream.write = function writeWithCapture(chunk, encoding, callback) {
    const text = stringifyChunk(chunk);
    emit({
      kind: "terminal_write",
      stream: name,
      bytes: Buffer.byteLength(text),
      text: trimText(text),
    });
    return originalWrite(chunk, encoding, callback);
  };
}

function installReadHook(stream, name) {
  const originalEmit = stream.emit.bind(stream);
  stream.emit = function emitWithCapture(eventName, ...args) {
    if (eventName === "data" && args.length > 0) {
      const text = stringifyChunk(args[0]);
      emit({
        kind: "terminal_read",
        stream: name,
        bytes: Buffer.byteLength(text),
        text: trimText(text),
      });
    }
    return originalEmit(eventName, ...args);
  };
}

function installFetchHook() {
  if (typeof globalThis.fetch !== "function") return;

  const originalFetch = globalThis.fetch.bind(globalThis);
  globalThis.fetch = async function fetchWithCapture(input, init = {}) {
    const requestInfo =
      typeof Request !== "undefined" && input instanceof Request
        ? {
            url: redactUrl(input.url),
            method: init.method || input.method,
            headers: headersToObject(init.headers || input.headers),
          }
        : {
            url: redactUrl(input),
            method: init.method || "GET",
            headers: headersToObject(init.headers),
          };

    const nextInit = { ...init };
    if (typeof nextInit.body === "string") {
      const replacedBody = replaceBodyText(nextInit.body);
      if (replacedBody !== nextInit.body) {
        emit({
          kind: "fetch_request_body_replaced",
          url: requestInfo.url,
          from: trimText(process.env.AO_MITM_REPLACE_FROM),
          to: trimText(process.env.AO_MITM_REPLACE_TO),
        });
        nextInit.body = replacedBody;
      }
    }

    emit({
      kind: "fetch_request",
      ...requestInfo,
      body: summarizeBody(nextInit.body),
    });

    const response = await originalFetch(input, nextInit);
    emit({
      kind: "fetch_response",
      url: requestInfo.url,
      status: response.status,
      statusText: response.statusText,
      headers: headersToObject(response.headers),
    });
    summarizeResponseBody(response, requestInfo);
    return response;
  };
}

function installWebSocketHook() {
  const OriginalWebSocket = globalThis.WebSocket;
  if (typeof OriginalWebSocket !== "function") return;

  globalThis.WebSocket = class CapturedWebSocket extends OriginalWebSocket {
    constructor(url, protocols) {
      emit({ kind: "websocket_open", url: redactUrl(url), protocols });
      super(url, protocols);

      this.addEventListener("message", (event) => {
        emit({
          kind: "websocket_receive",
          url: redactUrl(url),
          data: summarizeBody(event.data),
        });
      });
    }

    send(data) {
      emit({
        kind: "websocket_send",
        url: redactUrl(this.url),
        data: summarizeBody(data),
      });
      return super.send(data);
    }
  };
}

function installChildProcessHook() {
  for (const functionName of ["spawn", "spawnSync", "execFile", "execFileSync", "exec", "execSync"]) {
    const original = childProcess[functionName];
    if (typeof original !== "function") continue;

    childProcess[functionName] = function childProcessWithCapture(command, args, options, ...rest) {
      emit({
        kind: "child_process",
        functionName,
        command,
        args: Array.isArray(args) ? args : undefined,
        cwd: options && typeof options === "object" ? options.cwd : undefined,
      });
      return original.call(this, command, args, options, ...rest);
    };
  }
}

emit({
  kind: "preload_loaded",
  argv: process.argv,
  execArgv: process.execArgv,
  version: process.version,
});

installWriteHook(process.stdout, "stdout");
installWriteHook(process.stderr, "stderr");
installReadHook(process.stdin, "stdin");
installFetchHook();
installWebSocketHook();
installChildProcessHook();
