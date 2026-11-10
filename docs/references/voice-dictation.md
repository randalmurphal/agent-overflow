# Voice: dictation and read-aloud

Dictation has two sources, each a host-tier switch in Settings > Voice and
off by default. The subscription source sends a recording to ChatGPT's
transcription endpoint under the computer's Codex sign-in with ChatGPT.
The live source streams the microphone to OpenAI's live transcription with
an API key from an owner-only file (below). Where both are on and the
engine can stream, the composer dictates live.

Subscription source code: `internal/provider/codex/transcribe.go`
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

The level meter reads about 21 ms of the microphone every 100 ms, so a
short sound can fall between its reads. When the meter heard nothing, the
recording itself is decoded at 48 kHz and measured over stretches as
long as the meter's, starting at every sample (`recordingMeasurer` in
`browserDictation.ts`), and only one that
holds no sound is dropped as "No speech heard." A recording the engine
cannot decode within five seconds is sent, and the computer decides. One
recording is decoded at a time. A recording over a minute is not decoded,
since decoding holds it as samples several times over; the meter's verdict
stands for it.

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

## Live transcription with an API key

Code: `internal/openaikey` (the key file), `internal/openai/live.go` (the
socket), `internal/dictation/live.go` (sessions per connection),
`internal/app/app_live_dictation.go` (the bound methods),
`frontend/src/lib/dictation/pcmCapture.worklet.ts` and
`pcmDownsampler.ts` (capture) and `LiveAttempt` in
`dictationSession.svelte.ts`.

The key lives only in `<config root>/credentials/openai-api-key`, never in
settings or the environment. The reader opens the folder without following
links, then the file inside it the same way, refuses anything that is not
a regular file owned by this user, resets both to owner-only (0700, 0600),
and refuses a file that holds more than one key or more than a key's
bytes. The key is read for each connect into a buffer that is zeroed after
the handshake, and never reaches a client: `OpenAIKeyFile` reports only the
path and whether a usable key is there. Errors from OpenAI are scrubbed of
anything shaped like a key. Without host scope the settings page shows the
path; with it, Edit creates the file owner-only and opens it.

Verified 2026-10-22 against OpenAI's docs:

- `wss://api.openai.com/v1/realtime?intent=transcription` with
  `Authorization: Bearer`; one `session.update` of type `transcription`
  with `audio/pcm` at 24000 Hz, model `gpt-live-transcribe`, the delay
  (`minimal` to `xhigh`), languages, keywords and prompt from settings,
  and no turn detection.
- The client sends `input_audio_buffer.append` (base64 PCM) and, at stop,
  one `commit`; the server answers `committed`, then
  `conversation.item.input_audio_transcription.delta` and `.completed`
  for that item. A commit under 100 ms of audio is refused.
- OpenAI does not train on API data by default and keeps it up to 30 days
  for abuse monitoring; Settings > Voice says so.

The browser captures with an AudioWorklet, averages down to 24 kHz 16-bit
mono, and posts 100 ms pieces. Pieces queue while the computer connects
and go one append at a time, joined up to five seconds; each append answers
with the text heard so far. A failed connect or append ends the recording
at once. Stop flushes the worklet's part-filled piece, waits for the
connect and the last append, then finishes. The computer caps a session at
ten minutes of audio, four open at once, and closes one idle for 30 s; a
session belongs to the connection that opened it and closes with it.
Switching live off ends every live session at once, connecting ones
included, and the composer stops without an error. Silence is judged from
every sample sent, not from the sampled level meter, so a short word
counts.
Sources are re-read when the window regains focus, since the key file
changes outside the app.

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

Read-aloud speaks a finished reply sentence by sentence, in this device's
own voice or in an OpenAI voice made by the computer the reply's thread
runs on. With a device voice nothing leaves the device. Code:
`frontend/src/lib/utils/speakableText.ts` (what is read),
`frontend/src/lib/native/speech.ts` (the device engines),
`frontend/src/lib/native/clipSpeech.ts` (playback of fetched clips),
`frontend/src/lib/stores/readAloud.svelte.ts` (one reading at a time),
`frontend/src/lib/stores/readAloudSource.svelte.ts` (what each computer
offers) and
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
- The switch, voice, speed and volume are this device's, kept in frontend
  storage, and read-aloud is off until turned on. A saved OpenAI voice is
  `openai:<id>`.
- Each reading chooses its voice when it starts: the saved OpenAI voice
  when the reply's computer offers it, otherwise the device's voice (its
  default when the saved voice is an OpenAI one), otherwise the computer's
  first OpenAI voice on a device with none. A device with no voices (Arch's
  WebKitGTK is built without speech) shows no control unless the computer
  offers OpenAI voices.
- `Mod+Shift+>` pauses or resumes; starting dictation pauses a reading.

### OpenAI voices

With the host setting `readAloudAPIEnabled` on (off by default) and a key
in the API key file, a computer reads sentences in OpenAI voices.
`ReadAloudSource` reports availability and the voices;
`MintReadAloudTicket(text, voice)` admits one sentence (1 to 1000
characters after trimming) and returns
`/attachments/speech/{id}?ticket=...`. Fetching it runs the synthesis and
answers a 24 kHz mono 16-bit WAV, or a 4xx/5xx with a plain-text message
the client shows: 404 spent or expired, 409 no usable key, 429 busy or
rate limited, 502 an OpenAI failure, 504 over 45 seconds. 403 means the
switch is off or was written since the sentence was admitted, whether the
sentence was waiting or being read (the RPC says the same with code
`read_aloud_api_off`); a client stops on it without an error. Code:
`internal/openai/speech.go`, `internal/openai/fidelity.go`,
`internal/readaloud/`, `internal/app/app_read_aloud.go`,
`internal/transport/speechroutes.go`.

- One Realtime connection per sentence to `gpt-realtime-2.1-mini`
  (`wss://api.openai.com/v1/realtime?model=...`): a `session.update` of
  type `realtime` with audio-only output, no tools, minimal reasoning and
  turn detection off, then one out-of-band `response.create`
  (`conversation: "none"`) whose text is the JSON envelope
  `{"require_repeat_verbatim": true, "response_text": ...}`. The audio
  arrives as `response.output_audio.delta` and the model's own transcript
  as `response.output_audio_transcript.done`.
- The key is read from its file for each connection and zeroed when the
  handshake is done, as for live transcription.
- `openai.IsReading` compares the transcript with the text by words (by
  character in scripts written without spaces; a few short forms such
  as "OK" compare under their full spelling): a text of three or more
  words must keep more than half of them, and the words spoken beyond the
  text are bounded, with room for numbers spelled out. A result that is
  not a reading is retried once and then refused. Audio past 90 seconds
  is abandoned.
- At most 8 sentences wait and 4 synthesize per computer; a fifth
  synthesis is refused, not queued. A clip keeps its synthesis slot until
  its response is written, and a fetch is cut off after two minutes, so
  the audio held for slow readers is bounded too. An aborted fetch cancels
  the call.
- A write to the switch drops every sentence admitted before it and ends
  their syntheses, as it does live recordings; switching off and on again
  does not spare them. A sentence's id leads with its admission revision,
  so a dropped one is still answered 403.
- The client plays each WAV through one audio element from a blob URL,
  fetching one sentence ahead. The element is first started on a silent
  clip inside the click or key that began the reading, so engines that
  require a gesture allow the later plays. Speed is the element's
  `playbackRate` with pitch preserved.
- Voices: alloy, ash, ballad, coral, echo, sage, shimmer, verse, marin,
  cedar.

Measured 2026-11-10 with an API key over 113 calls: first audio about
0.5 s after the request (connect about 0.3 s more), a whole clip in 0.4 to
3.5 s, and the model's transcript equal to the text every time, including
text that asks a question or tells the model to do something else. 70 of
the clips were transcribed back with `gpt-transcribe`: 61 matched the
text, 4 differed by one word, and 5 were a sentence containing
`./internal/store`, which is spoken as "dot slash"; file paths and
similar symbols read poorly. Audio is billed at about 20 tokens per
second, about 2.6 cents per minute of speech at that date's prices.

Not verified: playback in WebKitGTK and WKWebView, and whether Android's
WebView keeps playing a reading while the app is in the background (the
device voice does, through the native queue).

## Other options researched (2026-08-04)

- Claude Code `/voice` (binary 2.1.219): TUI-only push-to-talk over
  `wss://<BASE_API_URL>/api/ws/speech_to_text/voice_stream`, Claude.ai OAuth,
  entitlement-gated. Internal API borrowing the CLI's rotating token.
- Codex app-server `thread/realtime/*`: thread-scoped full realtime session;
  `realtime_api_key()` in `core/src/realtime_conversation.rs` rejected
  ChatGPT sign-ins at research time.
- OpenAI API transcription needs an API key; it is the live source above.

Researched 2026-11-10 for read-aloud:

- OpenAI's speech endpoint (`/v1/audio/speech`; `tts-1`, `tts-1-hd`,
  `gpt-4o-mini-tts`) shuts down 2027-01-06, and OpenAI names
  `gpt-realtime-2.1-mini` over the Realtime API as its replacement. That
  is why read-aloud speaks through a Realtime session.
- ChatGPT sign-in realtime voice
  (`POST https://chatgpt.com/backend-api/codex/realtime/calls`, WebRTC,
  model `gpt-live-1-codex`): it speaks only while microphone audio flows,
  takes text through `session.context.append` as context rather than as a
  script, and refuses `response.create` for a ChatGPT sign-in. Over about
  130 calls, 3 of 8 spaced calls stayed silent and 4 of 5 that spoke
  answered the text instead of reading it. Not usable as a reader.
