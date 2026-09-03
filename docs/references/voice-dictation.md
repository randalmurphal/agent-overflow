# Voice dictation: researched options

Research from 2026-08-04 for voice-to-text input riding the user's existing
Claude or ChatGPT subscription, without local models. Nothing here is
built. Re-verify the binary facts before acting on them.

## Claude Code voice mode (verified in binary 2.1.219)

`/voice` push-to-talk dictation, TUI only (not in stream-json or the Agent
SDK). The backend is pure STT over WebSocket:
`wss://<BASE_API_URL>/api/ws/speech_to_text/voice_stream` (env override
`VOICE_STREAM_BASE_URL`), 16kHz linear16 PCM up, interim and final
transcripts back, Deepgram-style control messages (`KeepAlive`,
`CloseStream`, `TranscriptEndpoint`). Auth is the Claude.ai OAuth token
(subscription; "requires a Claude.ai account"), server-gated by the account
entitlement `allow_voice_mode` / `voice_mode_allowed`. Undocumented and
internal: using it from AO means borrowing the CLI's OAuth token for a
non-public API, which can break silently and touches the credential
rotation class that has bricked logins before.

Re-verify: `strings ~/.local/share/claude/versions/<ver> | grep voice_stream`.

## Codex app-server `thread/realtime/*` (experimental)

In the protocol since about May 2026 (0.142.5 repo, 0.146.0 CLI):
start / appendAudio / appendText / appendSpeech / stop / listVoices plus
transcript delta and done notifications. Transcripts arrive as plain user
text, so with `client_managed_handoffs: true`, `include_startup_context:
false`, and a prompt override it is usable as a dictation source routed
anywhere, Claude included. Caveats: thread-scoped (needs a live Codex
thread) and runs a full realtime-model session, not cheap STT. Blocked at
research time: `realtime_api_key()` in `core/src/realtime_conversation.rs`
rejects ChatGPT sign-in sessions and requires an API key, with a TODO
calling that temporary. Watch that function on new tags (the local checkout
lags the installed CLI; check upstream).

## If it is ever built

BYOK streaming STT in Go: mic capture in the webview, PCM over the
transport, Go forwards to the provider, key in settings behind a
host-scoped method. Default candidate: Deepgram Nova streaming (its WS
protocol has the same shape as Anthropic's `voice_stream`, so a later swap
to an official Claude endpoint is near drop-in). Alternatives: OpenAI
`gpt-4o(-mini)-transcribe` streaming on the API-key surface; Groq Whisper
turbo, batch per utterance only. Wispr Flow is not an integration target:
it is a system-wide app that already types into AO's fields.
