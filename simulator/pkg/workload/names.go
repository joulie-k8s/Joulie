package workload

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// Object names of a v1 run. A Job object, or with OwnerPod a bare pod, is
// named sim-<key>-<hash>: key is the group key sanitized to a DNS label and
// cut to MaxNameKeyLen, hash the first 8 hex characters of FNV-64a over the
// run id and the group key. The name is at most MaxObjectNameLen characters,
// so the hostname <job>-<completion index> of an Indexed pod stays within the
// 63 characters of a DNS label. Keys that sanitize or cut to the same text,
// and one key in two runs, are told apart by the 32-bit hash alone.
const (
	NamePrefix       = "sim-"
	MaxNameKeyLen    = 37
	MaxObjectNameLen = 50
)

// ObjectName is the name of the Job object of the group groupKey in run
// runID, or with OwnerPod the name of the pod of the job whose id is groupKey.
func ObjectName(runID, groupKey string) string {
	h := fnv.New64a()
	h.Write([]byte(runID))
	// The separator keeps ("a_", "b") and ("a", "_b") apart, whose keys
	// both sanitize to "b".
	h.Write([]byte{0})
	h.Write([]byte(groupKey))
	return fmt.Sprintf("%s%s-%08x", NamePrefix, sanitizeName(groupKey, MaxNameKeyLen), h.Sum64()>>32)
}

// GroupKey is the key of the group a trace job belongs to: its workloadId,
// podRole and submit offset as <workloadId>/<podRole>@<offset seconds>, or
// its jobId alone when it has no workloadId. The jobs of one key become one
// Job object unless their pods differ (GroupJobs).
func GroupKey(jobID, workloadID, podRole string, submitOffsetSec float64) string {
	if workloadID == "" {
		return jobID
	}
	return workloadID + "/" + podRole + "@" + strconv.FormatFloat(submitOffsetSec, 'f', -1, 64)
}

// sanitizeName lowercases in, turns every character outside [a-z0-9-] into
// '-', trims leading and trailing '-' and cuts the result to max characters
// without a trailing '-'. It returns "job" when nothing is left, as the
// legacy pod names do.
func sanitizeName(in string, max int) string {
	in = strings.ToLower(strings.TrimSpace(in))
	var b strings.Builder
	for _, r := range in {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > max {
		out = strings.TrimRight(out[:max], "-")
	}
	if out == "" {
		return "job"
	}
	return out
}

// labelValue is in as a label value: sanitized like a name and at most 63
// characters, or empty when in is empty, in which case the label is left out.
func labelValue(in string) string {
	if strings.TrimSpace(in) == "" {
		return ""
	}
	return sanitizeName(in, 63)
}
