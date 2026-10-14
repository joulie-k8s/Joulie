package workload

import (
	"fmt"
	"strings"
	"time"
)

// JobCompletedLogFormat is the simulator's completion log line. The
// experiments' 06_collect.py parse it with JOB_COMPLETED_RE, so legacy and
// v1 print it alike and its shape does not change.
const JobCompletedLogFormat = "job completed id=%s node=%s class=%s elapsed=%.1fs"

// FormatJobCompletedLog is the completion log line of a job, without the log
// prefix or a newline. elapsed runs from submission to completion; a negative
// one is written as 0, because the collector's pattern has no sign. The
// pattern reads the id, node and class each as one run of non-space
// characters, so an empty one is written as "unknown" and whitespace inside
// one as '_'.
func FormatJobCompletedLog(jobID, node, class string, elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	return fmt.Sprintf(JobCompletedLogFormat, logField(jobID), logField(node), logField(class), elapsed.Seconds())
}

// logField is s as one field of the completion line: its runs of whitespace
// joined by '_', or "unknown" when nothing else is left.
func logField(s string) string {
	if s = strings.Join(strings.Fields(s), "_"); s == "" {
		return "unknown"
	}
	return s
}
