package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"agent-overflow/internal/importir"
)

// profileAccumulator is the single definition of which rollout records carry
// model configuration. Both Parse and the repair-only scanner feed it, so the
// cheap recovery path cannot drift from ordinary imports as new wire shapes
// are added.
type profileAccumulator struct {
	value importir.ModelProfile
}

func (p *profileAccumulator) observeTurnContext(value turnContextPayload) {
	if value.Model != "" {
		p.value.Model = value.Model
	}
	if value.Effort != "" {
		p.value.ReasoningEffort = value.Effort
	}
}

func (p *profileAccumulator) observeTaskStarted(value taskStartedPayload) {
	if value.ModelContextWindow > 0 {
		p.value.ContextWindow = value.ModelContextWindow
	}
}

func (p *profileAccumulator) observeTokenCount(value tokenCountPayload) {
	if value.Info != nil && value.Info.ModelContextWindow > 0 {
		p.value.ContextWindow = value.Info.ModelContextWindow
	}
}

// ReadLatestProfile scans a rollout without constructing its event history.
// It exists for repairing imported rows created by older readers that lost a
// late turn_context. Normal imports obtain the same profile for free from
// Parse; callers should not add this second pass to that path.
func ReadLatestProfile(ctx context.Context, path string) (importir.ModelProfile, error) {
	file, err := os.Open(path)
	if err != nil {
		return importir.ModelProfile{}, fmt.Errorf("rollout: open %s for profile: %w", path, err)
	}
	defer file.Close()

	var profile profileAccumulator
	scanner := newScanner(file, 0, DefaultMaxLineBytes, scanBufferSize)
	for lines := 0; ; lines++ {
		if lines%512 == 0 {
			if err := ctx.Err(); err != nil {
				return importir.ModelProfile{}, err
			}
		}
		line, err := scanner.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return importir.ModelProfile{}, fmt.Errorf("rollout: read %s for profile: %w", path, err)
		}
		if line.Oversized {
			continue
		}
		env, ok := decodeEnvelope(line.Data)
		if !ok {
			continue
		}
		switch env.Type {
		case typeTurnContext:
			var value turnContextPayload
			if json.Unmarshal(env.Payload, &value) == nil {
				profile.observeTurnContext(value)
			}
		case typeEventMsg:
			switch payloadType(env.Payload) {
			case "task_started":
				var value taskStartedPayload
				if json.Unmarshal(env.Payload, &value) == nil {
					profile.observeTaskStarted(value)
				}
			case "token_count":
				var value tokenCountPayload
				if json.Unmarshal(env.Payload, &value) == nil {
					profile.observeTokenCount(value)
				}
			}
		}
	}
	return profile.value, nil
}
