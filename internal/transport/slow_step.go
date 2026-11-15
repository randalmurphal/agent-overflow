package transport

import (
	"net/http"
	"time"

	"agent-overflow/internal/logging"
)

// Routes a paired computer's credential, manifest and socket steps reach,
// as logged when one answers slowly (logging.LogSlowStep).
const (
	slowStepComponent = "transport"
	stepPairRoute     = "pair"
	stepRenewRoute    = "renew"
	stepTicketRoute   = "ticket"
	stepBootstrap     = "bootstrap"
	stepHealth        = "health"
	stepWSAccept      = "ws_accept"
)

// timedRoute logs an answer that outlasted the slow-step threshold, timed
// from the handler's start (the request line and headers read) to its
// return (the response written), with the status it wrote. Outermost on
// its route, so a rate-limited or guarded refusal is timed too.
func timedRoute(step string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next(recorder, r)
		logging.LogSlowStep(slowStepComponent, step, started, logging.StatusOutcome(recorder.written()))
	}
}

// statusRecorder remembers the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(body []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(body)
}

// Unwrap keeps http.ResponseController reaching the connection.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) written() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}
