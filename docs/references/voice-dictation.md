# Voice: dictation and read-aloud

Dictation sends a recording to ChatGPT's transcription endpoint under the
computer's Codex sign-in with ChatGPT. Code: `internal/provider/codex/transcribe.go`
(the request), `internal/provideraccountapp/dictation.go` (the credential
and its refresh), `internal/dictation` (pieces held until the transcription
ends) and `frontend/src/lib/dictation` (recording). Re-verify the facts
below before changing the request.

It is off until Settings > Voice turns it on for that computer (host tier
`dictationSubscriptionEnabled`). With no source on, no composer shows a
mic and the computer refuses to open or transcribe a recording.

## ChatGPT `backend-api/transcribe`

`POST https://chatgpt.com/backend-api/transcribe`, multipart with one `file`
part, answered `{"text": ...}`. Headers: `Authorization: Bearer <access
token>`, `ChatGPT-Account-Id`, Codex's `originator` and User-Agent. The
Codex TUI used it for dictation between 2026-02-23 and 2026-03-29
(`codex-rs/tui/src/voice.rs`, commits `855e275591`, `bc0a5843df`), with a
24 kHz mono 16-bit WAV named `audio.wav`.

Spike 2026-10-15 with a Pro-plan Codex login: a 4.4 s WAV returned 200 in
0.79 s with the exact sentence. The response also carried `asset_pointer:
sediment://file_...` and `asset_ttl: 30d`: ChatGPT keeps each recording for
30 days, which Settings → Voice states.

AO uploads what MediaRecorder produces: WebM/Opus in Chromium and Firefox,
MP4 in WebKit, named `audio.webm` or `audio.mp4`. Check 2026-10-21: the
same sentence as a 12 KB browser-recorded WebM/Opus clip, sent through
`codex.Transcribe`, returned the exact text in 0.58 s. MP4 has not been
sent to the live endpoint.

The access token is read from the canonical Codex `auth.json` for each
request and never retained. On a 401, AO asks Codex to refresh through
`account/read {refreshToken: true}` and retries once; AO never refreshes the
token itself. The refresh holds the Codex reconcile lock and first promotes
a rotation recovered into the selected account's slot, so it never spends a
consumed refresh token. A transcription abandoned before it holds the lock
starts no refresh, and a refresh that started runs to its end: one stopped
after the server rotated the token, before the CLI saved it, would lose the
sign-in. A Codex configured to keep credentials in the OS keyring has
no `auth.json`, and dictation reads as unavailable there.

## Platform microphone access

| Surface | Permission |
|---|---|
| Linux desktop (WebKitGTK 6.0) | Granted to the app's page by the Wails permission handler; `enable-media-stream` defaults on |
| Windows desktop (WebView2) | Granted by Wails's global allow |
| macOS desktop (WKWebView) | `NSMicrophoneUsageDescription` in both plists. Wails sets no media-capture delegate; whether WKWebView then prompts per origin is unverified |
| Android shell | `RECORD_AUDIO` and `MODIFY_AUDIO_SETTINGS` in the manifest; Capacitor's `BridgeWebChromeClient` requests both at runtime. After two refusals Android stops asking |
| Connected browser | The browser's own prompt; needs a secure context |

An engine whose MediaRecorder takes neither type shows no mic. Recording on
macOS WKWebView, iOS Safari and WebKitGTK is untested here: Playwright's
WebKit needs GStreamer libraries this development host lacks.

## Read aloud

Read-aloud speaks a finished reply in this device's own voice; nothing
leaves the device. Code: `frontend/src/lib/utils/speakableText.ts` (what is
read), `frontend/src/lib/native/speech.ts` (the engines),
`frontend/src/lib/stores/readAloud.svelte.ts` (one reading at a time) and
`mobile/android/app/src/main/java/dev/agentoverflow/app/speech/`.

- The reply is read from its parsed markdown as sentences. Code blocks,
  display math, tables, raw HTML, images, bare URLs, footnote markers and
  emoji are skipped silently, as Open WebUI does; link text and inline code
  are read as words.
- Each sentence is one utterance, capped at 280 characters (cut at clause
  punctuation, then whitespace), so no engine stalls on a long one.
- Pause stops the voice and keeps the sentence; resume starts that
  sentence again. Mid-sentence resume is unreliable across OS voices.
- Browsers and the desktop webviews speak through `speechSynthesis`.
  Android System WebView has none (reports through WebView 138), so the
  shell speaks through Android's TextToSpeech, queueing every sentence at
  once so a reading continues in the background. Package visibility needs
  the `TTS_SERVICE` query in the manifest.
- A device with no voices (Arch's WebKitGTK is built without speech) shows
  no control. The switch, voice, speed and volume are this device's,
  kept in frontend storage, and read-aloud is off until turned on.
- `Mod+Shift+>` pauses or resumes; starting dictation pauses a reading.

## Other options researched (2026-08-04)

- Claude Code `/voice` (binary 2.1.219): TUI-only push-to-talk over
  `wss://<BASE_API_URL>/api/ws/speech_to_text/voice_stream`, Claude.ai OAuth,
  entitlement-gated. Internal API borrowing the CLI's rotating token.
- Codex app-server `thread/realtime/*`: thread-scoped full realtime session;
  `realtime_api_key()` in `core/src/realtime_conversation.rs` rejected
  ChatGPT sign-ins at research time.
- OpenAI API transcription (`gpt-4o-transcribe` family) needs an API key;
  it is the planned key-file mode.
