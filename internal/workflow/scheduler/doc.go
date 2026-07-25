// Package scheduler turns workflow automations (spec §11) into run starts: it
// owns the cron timer and the internal-event matching, decides whether a fired
// trigger becomes a run, and records every fire it deliberately did not start.
package scheduler
