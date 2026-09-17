package app

import (
	"log"

	"agent-overflow/internal/assetwatch"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/soundlib"
)

// soundService returns the lazy-initialized sounds-directory service.
// Construction is one-shot — subsequent calls reuse the same Service (and
// its mutex), matching themeService and spinnerService.
func (a *App) soundService() (*soundlib.Service, error) {
	a.soundOnce.Do(func() {
		a.sound, a.soundErr = soundlib.New(a.configDir)
	})
	return a.sound, a.soundErr
}

// GetSoundFiles returns every custom notification cue in
// <configDir>/sounds: the directory it lives in, each validated cue as
// base64 WAV bytes, and the reasons any file could not be used.
//
// The library belongs to the BACKEND HOST, not to a screen: a cue is a file
// on the machine the notification gate runs on, and every screen attached to
// that machine picks from the same list. That is why this is one settings-read
// RPC routed home rather than a frontend-local asset library like themes and
// spinners, whose selection is per client.
//
// Every file is re-validated on every call (internal/soundlib). The error
// return covers service construction only (no writable config path);
// per-file problems are Warnings on the result — user-facing state, not log
// entries.
//
//ao:scope settings:read
//ao:route home
func (a *App) GetSoundFiles() (soundlib.Files, error) {
	service, err := a.soundService()
	if err != nil {
		return soundlib.Files{}, err
	}
	return service.Files(), nil
}

// PutSoundFile stores one custom cue under a new id.
//
// The WAV arrives ALREADY RENDERED by the frontend: the browser decoded
// whatever the user picked with the engine's own sandboxed decoder, mixed it
// to mono, resampled it and encoded the canonical file. Nothing the user
// supplied is stored as supplied. This side does not trust that pipeline
// either — soundlib.Put validates the bytes structurally before they are
// written, and the listing validates them again on every read.
//
// settings:write rather than host: the library is shared configuration for
// the backend machine, the same class as the settings keys that name a cue,
// and a paired device editing it is editing a list it can also see.
//
//ao:scope settings:write
//ao:route home
func (a *App) PutSoundFile(id string, wavBase64 string) error {
	service, err := a.soundService()
	if err != nil {
		return err
	}
	// The size is checked before the decode, inside soundlib.DecodeWAV.
	wav, err := soundlib.DecodeWAV(id, wavBase64)
	if err != nil {
		return err
	}
	// Path validates the id, so a wire string can never reach the
	// suppression ledger or a filesystem path built from it.
	path, err := service.Path(id)
	if err != nil {
		return err
	}
	a.suppressSoundWatch(path)
	defer a.suppressSoundWatch(path)
	if err := service.Put(id, wav); err != nil {
		return err
	}
	// The watcher's event for this write is suppressed, so the listing
	// change has to be announced here or the OTHER screens attached to this
	// backend would keep showing the old library.
	a.emit(eventchan.SoundChanged, nil)
	return nil
}

// DeleteSoundFile removes one custom cue.
//
// A settings key still naming the deleted cue is deliberately left alone.
// The player falls back to that event's default cue and says so, which is
// recoverable; rewriting three device-tier keys across every screen attached
// to this backend, from a delete on one of them, is not.
//
//ao:scope settings:write
//ao:route home
func (a *App) DeleteSoundFile(id string) error {
	service, err := a.soundService()
	if err != nil {
		return err
	}
	path, err := service.Path(id)
	if err != nil {
		return err
	}
	a.suppressSoundWatch(path)
	defer a.suppressSoundWatch(path)
	if err := service.Delete(id); err != nil {
		return err
	}
	a.emit(eventchan.SoundChanged, nil)
	return nil
}

// startSoundWatcher arms the sounds-directory watcher. A watcher that cannot
// start is logged and skipped rather than failing boot: live reload is a
// convenience on top of GetSoundFiles, and the app is fully usable without
// it (the built-in cues ship with the frontend).
func (a *App) startSoundWatcher(dir string) {
	watcher, err := assetwatch.NewSoundWatcher(dir, func() {
		a.emit(eventchan.SoundChanged, nil)
	})
	if err != nil {
		log.Printf("sound watcher unavailable: %v", err)
		return
	}
	a.soundWatcher = watcher
}

// suppressSoundWatch marks a path as written by this process. Nil-safe so
// the write paths do not care whether the watcher started.
func (a *App) suppressSoundWatch(path string) {
	if a.soundWatcher == nil {
		return
	}
	a.soundWatcher.Suppress(path)
}
