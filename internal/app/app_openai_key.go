package app

import (
	"errors"
	"fmt"

	"agent-overflow/internal/openaikey"
	"agent-overflow/internal/secret"
)

// The OpenAI API key file. Settings > Voice names it and its state, and on
// this computer creates it and opens it in the user's editor. The key never
// crosses the wire: requests read it fresh for themselves
// (readOpenAIKey), and the state is all a client sees.

// OpenAIKeyFile describes this computer's OpenAI API key file, never the
// key.
type OpenAIKeyFile struct {
	Path  string          `json:"path"`
	State openaikey.State `json:"state"`
	// Problem says why a refused file is refused.
	Problem string `json:"problem,omitempty"`
}

func (a *App) openAIKeyPath() (string, error) {
	if a.configDir == "" {
		return "", errors.New("this computer has no config folder for an OpenAI API key")
	}
	return openaikey.Path(a.configDir), nil
}

func (a *App) openAIKeyFile() (OpenAIKeyFile, error) {
	path, err := a.openAIKeyPath()
	if err != nil {
		return OpenAIKeyFile{}, err
	}
	status := openaikey.Inspect(path)
	return OpenAIKeyFile{Path: path, State: status.State, Problem: status.Problem}, nil
}

// OpenAIKeyFile reports where this computer keeps its OpenAI API key and
// whether a usable key is there.
//
//ao:scope settings:read
//ao:route selected
func (a *App) OpenAIKeyFile() (OpenAIKeyFile, error) {
	return a.openAIKeyFile()
}

// EditOpenAIKeyFile creates the key file, empty and owner-only, when it is
// missing, and opens it in the user's editor on this computer.
//
//ao:scope host
//ao:route selected
func (a *App) EditOpenAIKeyFile() (OpenAIKeyFile, error) {
	path, err := a.openAIKeyPath()
	if err != nil {
		return OpenAIKeyFile{}, err
	}
	if _, err := openaikey.Create(path); err != nil {
		return OpenAIKeyFile{}, err
	}
	if err := a.OpenInEditor(path, 0, 0, "", ""); err != nil {
		return OpenAIKeyFile{}, fmt.Errorf("open the OpenAI API key file: %w", err)
	}
	return a.openAIKeyFile()
}

// readOpenAIKey reads the key for one request; the caller zeroes it.
func (a *App) readOpenAIKey() (*secret.Bytes, error) {
	path, err := a.openAIKeyPath()
	if err != nil {
		return nil, err
	}
	key, err := openaikey.Read(path)
	if errors.Is(err, openaikey.ErrMissing) || errors.Is(err, openaikey.ErrEmpty) {
		return nil, fmt.Errorf("put an OpenAI API key in %s to use it", path)
	}
	return key, err
}
