package jira

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// timeEstimateField is the system field a board estimates in when it
// estimates by original time; Jira keeps it in seconds.
const timeEstimateField = "timeoriginalestimate"

// Estimate reads is's estimate in the board's estimation field: points,
// or seconds for the original time estimate. set is false when the
// issue has none. The issue must have been read with the field
// (Estimation.FieldID); a board that estimates by no field is an error.
func (b BoardConfig) Estimate(is *Issue) (value float64, set bool, err error) {
	if b.Estimation.Type != EstimateField || b.Estimation.FieldID == "" {
		return 0, false, fmt.Errorf("jira: board %d estimates by %s, not by a field", b.BoardID, b.Estimation.Type)
	}
	n, err := is.Field(b.Estimation.FieldID).Number()
	if errors.Is(err, ErrNoValue) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("jira: the estimate of %s: %w", is.Key, err)
	}
	return n, true, nil
}

// EstimateValue is an estimate SetEstimate writes: Points for a board
// estimating in a number field, TimeEstimate for one estimating by
// original time, or NoEstimate. The zero EstimateValue is refused.
type EstimateValue struct {
	kind   estimateKind
	points float64
	time   time.Duration
}

type estimateKind uint8

const (
	estimateUnset estimateKind = iota
	estimatePoints
	estimateTime
	estimateNone
)

// Points is an estimate in a number field, such as story points.
func Points(n float64) EstimateValue { return EstimateValue{kind: estimatePoints, points: n} }

// TimeEstimate is an original time estimate, in whole minutes.
func TimeEstimate(d time.Duration) EstimateValue { return EstimateValue{kind: estimateTime, time: d} }

// NoEstimate clears the estimate.
func NoEstimate() EstimateValue { return EstimateValue{kind: estimateNone} }

// EstimateInput sets Issue's estimate through Board, which decides the
// field written.
type EstimateInput struct {
	Issue string
	Board int64
	Value EstimateValue
}

// SetEstimate writes an issue's estimate through the board's estimation
// field, whether or not the field is on the issue's screens. The board's
// configuration is read first: a board that estimates by no field, or a
// value of the other kind than the field takes, is refused before
// anything is sent. It returns the field ID and the value as Jira stored
// it; once the write was sent, a failure still returns the board's field
// ID, and an answer that does not decode is a StepEstimate *StepError
// whose Applied reports it.
func (c *Client) SetEstimate(ctx context.Context, in EstimateInput) (fieldID, stored string, err error) {
	if err := c.admit(ctx); err != nil {
		return "", "", err
	}
	if err := checkIssue(in.Issue); err != nil {
		return "", "", err
	}
	cfg, err := c.boardConfig(ctx, in.Board)
	if err != nil {
		return "", "", notSentErr(err)
	}
	value, err := estimateText(cfg, in.Value)
	if err != nil {
		return "", "", notSentErr(fmt.Errorf("jira: estimate %s: %w", in.Issue, err))
	}
	var j struct {
		FieldID string `json:"fieldId"`
		Value   jsonID `json:"value"`
	}
	_, err = c.call(ctx, trackerapi.Request{
		Method: http.MethodPut, Path: agilePath + "issue/" + url.PathEscape(in.Issue) + "/estimation",
		Query: url.Values{"boardId": {strconv.FormatInt(in.Board, 10)}},
		Body:  map[string]any{"value": value},
	}, &j)
	if err != nil {
		return cfg.Estimation.FieldID, "", fmt.Errorf("jira: estimate %s: %w", in.Issue, &StepError{Step: StepEstimate, Err: err})
	}
	return cmp.Or(j.FieldID, cfg.Estimation.FieldID), string(j.Value), nil
}

// estimateText is v as the estimation endpoint takes it for cfg's field:
// a number, minutes ("90m") for original time, or nil to clear.
func estimateText(cfg BoardConfig, v EstimateValue) (*string, error) {
	if cfg.Estimation.Type != EstimateField || cfg.Estimation.FieldID == "" {
		return nil, fmt.Errorf("board %d estimates by %s, not by a field", cfg.BoardID, cfg.Estimation.Type)
	}
	byTime := cfg.Estimation.FieldID == timeEstimateField
	var s string
	switch v.kind {
	case estimateNone:
		return nil, nil
	case estimatePoints:
		if byTime {
			return nil, fmt.Errorf("board %d estimates by time; give a TimeEstimate", cfg.BoardID)
		}
		if math.IsNaN(v.points) || math.IsInf(v.points, 0) || v.points < 0 {
			return nil, fmt.Errorf("%v is not an estimate", v.points)
		}
		s = strconv.FormatFloat(v.points, 'f', -1, 64)
	case estimateTime:
		if !byTime {
			return nil, fmt.Errorf("board %d estimates in %s; give Points", cfg.BoardID, cfg.Estimation.FieldName)
		}
		if v.time <= 0 || v.time%time.Minute != 0 {
			return nil, fmt.Errorf("%s is not a whole number of minutes", v.time)
		}
		s = strconv.FormatInt(int64(v.time/time.Minute), 10) + "m"
	default:
		return nil, errors.New("no estimate given")
	}
	return &s, nil
}
