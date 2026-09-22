package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestFacilityMetricsGetReturnsZerosWhenNotSet(t *testing.T) {
	t.Parallel()
	fm := &facilityMetrics{}
	ambient, itPower, cooling := fm.get()
	if ambient != 0 || itPower != 0 || cooling != 0 {
		t.Fatalf("expected zeros, got ambient=%v itPower=%v cooling=%v", ambient, itPower, cooling)
	}
}

func TestFacilityMetricsSetAndGet(t *testing.T) {
	t.Parallel()
	fm := &facilityMetrics{}
	fm.set(25.5, 10000, 3000)

	ambient, itPower, cooling := fm.get()
	if ambient != 25.5 {
		t.Fatalf("ambient=%v want=25.5", ambient)
	}
	if itPower != 10000 {
		t.Fatalf("itPower=%v want=10000", itPower)
	}
	if cooling != 3000 {
		t.Fatalf("cooling=%v want=3000", cooling)
	}
}

func TestFacilityMetricsOverwrite(t *testing.T) {
	t.Parallel()
	fm := &facilityMetrics{}
	fm.set(20, 5000, 1000)
	fm.set(30, 8000, 2500)

	ambient, itPower, cooling := fm.get()
	if ambient != 30 || itPower != 8000 || cooling != 2500 {
		t.Fatalf("overwrite failed: ambient=%v itPower=%v cooling=%v", ambient, itPower, cooling)
	}
}

func TestFacilityMetricsConcurrentAccess(t *testing.T) {
	t.Parallel()
	fm := &facilityMetrics{}
	var wg sync.WaitGroup

	// Concurrent writers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(v float64) {
			defer wg.Done()
			fm.set(v, v*100, v*50)
		}(float64(i))
	}

	// Concurrent readers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fm.get()
		}()
	}

	wg.Wait()
	// If we get here without a race panic, the mutex is working.
	// Do a final read to confirm the data is consistent.
	ambient, itPower, cooling := fm.get()
	if itPower != ambient*100 || cooling != ambient*50 {
		t.Fatalf("inconsistent state: ambient=%v itPower=%v cooling=%v", ambient, itPower, cooling)
	}
}

func TestQueryPrometheusScalarSuccess(t *testing.T) {
	t.Parallel()
	response := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"result": []interface{}{
				map[string]interface{}{
					"value": []interface{}{1234567890.0, "42.5"},
				},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := server.Client()
	val, err := queryPrometheusScalar(context.Background(), client, server.URL, "test_metric")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != 42.5 {
		t.Fatalf("val=%v want=42.5", val)
	}
}

func TestQueryPrometheusScalarEmptyResult(t *testing.T) {
	t.Parallel()
	response := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"result": []interface{}{},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := server.Client()
	_, err := queryPrometheusScalar(context.Background(), client, server.URL, "missing_metric")
	if err == nil {
		t.Fatal("expected error for empty result")
	}
}

func TestQueryPrometheusScalarErrorStatus(t *testing.T) {
	t.Parallel()
	response := map[string]interface{}{
		"status": "error",
		"data": map[string]interface{}{
			"result": []interface{}{},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := server.Client()
	_, err := queryPrometheusScalar(context.Background(), client, server.URL, "test_metric")
	if err == nil {
		t.Fatal("expected error for error status")
	}
}

func TestQueryPrometheusScalarInvalidJSON(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer server.Close()

	client := server.Client()
	_, err := queryPrometheusScalar(context.Background(), client, server.URL, "test_metric")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestQueryPrometheusScalarHTTPError(t *testing.T) {
	t.Parallel()
	// Use a URL that will fail to connect.
	client := &http.Client{}
	_, err := queryPrometheusScalar(context.Background(), client, "http://127.0.0.1:0", "test_metric")
	if err == nil {
		t.Fatal("expected error for connection failure")
	}
}

func TestQueryPrometheusScalarNonStringValue(t *testing.T) {
	t.Parallel()
	response := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"result": []interface{}{
				map[string]interface{}{
					"value": []interface{}{1234567890.0, 42.5},
				},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := server.Client()
	_, err := queryPrometheusScalar(context.Background(), client, server.URL, "test_metric")
	if err == nil {
		t.Fatal("expected error when value is not a string")
	}
}
