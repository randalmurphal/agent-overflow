# `internal/chatmodel`

Pure helpers for chat model profiles, capability checks, and context-window validation. Persistence and frontend wire types stay in `internal/app`; live Codex catalog state stays in `internal/codexmodels`.

- Sanitize stored profiles before projecting them. Registry changes can make persisted context-window values stale.
- Do not judge fast mode here. Which models run fast is what the provider reports; the caller's merged catalog owns it and the stored flag passes through.
- Keep Codex permissive when a live catalog model is absent from the static registry.
- User-facing context-window decisions must use caller-supplied merged catalog options. Static registry lookups are fallback data, not authority.
- Keep the package free of App and store handles.
