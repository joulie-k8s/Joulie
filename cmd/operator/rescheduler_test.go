package main

import (
	"testing"
)

func TestExtractRescheduleRecommendationsParses(t *testing.T) {
	t.Parallel()
	obj := map[string]interface{}{
		"status": map[string]interface{}{
			"rescheduleRecommendations": []interface{}{
				map[string]interface{}{
					"workloadRef": map[string]interface{}{
						"namespace": "prod",
						"name":      "web-server-abc",
					},
					"reason": "thermal-stress",
				},
				map[string]interface{}{
					"workloadRef": map[string]interface{}{
						"namespace": "staging",
						"name":      "batch-job-xyz",
					},
					"reason": "psu-overload",
				},
			},
		},
	}

	recs := extractRescheduleRecommendations(obj)
	if len(recs) != 2 {
		t.Fatalf("len(recs)=%d want=2", len(recs))
	}

	if recs[0].namespace != "prod" {
		t.Fatalf("recs[0].namespace=%q want=prod", recs[0].namespace)
	}
	if recs[0].podName != "web-server-abc" {
		t.Fatalf("recs[0].podName=%q want=web-server-abc", recs[0].podName)
	}
	if recs[0].reason != "thermal-stress" {
		t.Fatalf("recs[0].reason=%q want=thermal-stress", recs[0].reason)
	}

	if recs[1].namespace != "staging" {
		t.Fatalf("recs[1].namespace=%q want=staging", recs[1].namespace)
	}
	if recs[1].podName != "batch-job-xyz" {
		t.Fatalf("recs[1].podName=%q want=batch-job-xyz", recs[1].podName)
	}
	if recs[1].reason != "psu-overload" {
		t.Fatalf("recs[1].reason=%q want=psu-overload", recs[1].reason)
	}
}

func TestExtractRescheduleRecommendationsEmptyStatus(t *testing.T) {
	t.Parallel()
	obj := map[string]interface{}{
		"status": map[string]interface{}{},
	}
	recs := extractRescheduleRecommendations(obj)
	if len(recs) != 0 {
		t.Fatalf("len(recs)=%d want=0", len(recs))
	}
}

func TestExtractRescheduleRecommendationsNoStatus(t *testing.T) {
	t.Parallel()
	obj := map[string]interface{}{}
	recs := extractRescheduleRecommendations(obj)
	if len(recs) != 0 {
		t.Fatalf("len(recs)=%d want=0", len(recs))
	}
}

func TestExtractRescheduleRecommendationsSkipsInvalidItems(t *testing.T) {
	t.Parallel()
	obj := map[string]interface{}{
		"status": map[string]interface{}{
			"rescheduleRecommendations": []interface{}{
				"not-a-map",
				map[string]interface{}{
					"workloadRef": "not-a-map-either",
				},
				map[string]interface{}{
					"workloadRef": map[string]interface{}{
						"namespace": "valid-ns",
						"name":      "valid-pod",
					},
					"reason": "valid-reason",
				},
			},
		},
	}

	recs := extractRescheduleRecommendations(obj)
	if len(recs) != 1 {
		t.Fatalf("len(recs)=%d want=1 (only valid entry)", len(recs))
	}
	if recs[0].namespace != "valid-ns" || recs[0].podName != "valid-pod" || recs[0].reason != "valid-reason" {
		t.Fatalf("unexpected rec: %+v", recs[0])
	}
}

func TestExtractSchedulableClass(t *testing.T) {
	t.Parallel()
	obj := map[string]interface{}{
		"status": map[string]interface{}{
			"schedulableClass": "eco",
		},
	}
	got := extractSchedulableClass(obj)
	if got != "eco" {
		t.Fatalf("extractSchedulableClass=%q want=eco", got)
	}
}

func TestExtractSchedulableClassMissing(t *testing.T) {
	t.Parallel()
	obj := map[string]interface{}{
		"status": map[string]interface{}{},
	}
	got := extractSchedulableClass(obj)
	if got != "" {
		t.Fatalf("extractSchedulableClass=%q want empty", got)
	}
}

func TestExtractRescheduleRecommendationsMissingFields(t *testing.T) {
	t.Parallel()
	obj := map[string]interface{}{
		"status": map[string]interface{}{
			"rescheduleRecommendations": []interface{}{
				map[string]interface{}{
					"workloadRef": map[string]interface{}{
						// namespace and name are missing
					},
					// reason is missing
				},
			},
		},
	}

	recs := extractRescheduleRecommendations(obj)
	if len(recs) != 1 {
		t.Fatalf("len(recs)=%d want=1", len(recs))
	}
	// Fields should be empty strings when missing.
	if recs[0].namespace != "" {
		t.Fatalf("namespace=%q want empty", recs[0].namespace)
	}
	if recs[0].podName != "" {
		t.Fatalf("podName=%q want empty", recs[0].podName)
	}
	if recs[0].reason != "" {
		t.Fatalf("reason=%q want empty", recs[0].reason)
	}
}
