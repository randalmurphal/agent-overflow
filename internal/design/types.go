package design

import "agent-overflow/internal/store"

// DesignArtifact is the public design-mode artifact metadata shape.
type DesignArtifact = store.DesignArtifact

// DesignOption is one selectable design direction presented to the user.
type DesignOption struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	ArtifactID  string `json:"artifactId"`
}

// DesignOptionsRequest is emitted when the agent needs the user to choose
// between multiple design directions.
type DesignOptionsRequest struct {
	RequestID string         `json:"requestId"`
	ThreadID  string         `json:"threadId"`
	Prompt    string         `json:"prompt"`
	Options   []DesignOption `json:"options"`
}

// RenderInput is the tool payload for a rendered HTML artifact.
type RenderInput struct {
	HTML        string `json:"html"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// PresentOptionInput is one HTML-backed option shown to the user.
type PresentOptionInput struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	HTML        string `json:"html"`
}

// PresentOptionsInput is the tool payload for a design-choice request.
type PresentOptionsInput struct {
	Prompt  string               `json:"prompt"`
	Options []PresentOptionInput `json:"options"`
}

// ChoiceResult is returned to the provider once the user selects an option.
type ChoiceResult struct {
	Chosen string `json:"chosen"`
	Title  string `json:"title"`
}
