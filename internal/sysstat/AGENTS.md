# Host system statistics

This package is a read-only adapter around gopsutil for CPU and memory samples.
Application code owns cadence, goroutines, and event emission.

Prime CPU sampling once before periodic reads because the library keeps
process-global delta state. Preserve the selected memory-used convention on the
wire. Return sampling errors instead of partial silent values.

Tests replace the package read functions; do not depend on the developer
machine's load or memory.
