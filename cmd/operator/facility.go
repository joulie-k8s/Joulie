package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// facilityMetrics holds data-center-level metrics fetched from Prometheus.
// These feed into the twin computation for PUE estimation and cooling stress.
// When topology is enabled, per-zone ambient temperatures and per-rack power
// draws are also tracked.
type facilityMetrics struct {
	mu                sync.RWMutex
	ambientTempC      float64
	totalITPowerW     float64
	coolingPowerW     float64
	zoneAmbientTempC  map[string]float64 // cooling-zone -> ambient temp
	rackPowerW        map[string]float64 // rack -> total power draw
	lastUpdated       time.Time
}

func (f *facilityMetrics) get() (ambientC, itPowerW, coolingW float64) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.ambientTempC, f.totalITPowerW, f.coolingPowerW
}

func (f *facilityMetrics) set(ambientC, itPowerW, coolingW float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ambientTempC = ambientC
	f.totalITPowerW = itPowerW
	f.coolingPowerW = coolingW
	f.lastUpdated = time.Now()
}

// getZoneAmbient returns the ambient temperature for a specific cooling zone.
// Falls back to the global ambient if per-zone data is not available.
func (f *facilityMetrics) getZoneAmbient(zone string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if zone != "" {
		if v, ok := f.zoneAmbientTempC[zone]; ok {
			return v
		}
	}
	return f.ambientTempC
}

// getRackPower returns the total power draw for a specific rack.
// Returns 0 if per-rack data is not available (caller falls back to cluster-wide).
func (f *facilityMetrics) getRackPower(rack string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if rack != "" {
		if v, ok := f.rackPowerW[rack]; ok {
			return v
		}
	}
	return 0
}

// setTopology stores per-zone and per-rack metrics.
func (f *facilityMetrics) setTopology(zoneTemps map[string]float64, rackPower map[string]float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.zoneAmbientTempC = zoneTemps
	f.rackPowerW = rackPower
}

type facilityConfig struct {
	enabled           bool
	prometheusAddress string
	pollInterval      time.Duration
	// Configurable metric names for different Prometheus setups.
	ambientTempMetric  string
	itPowerMetric      string
	coolingPowerMetric string
	// Topology-aware metric templates. Use %s as placeholder for the
	// zone or rack label value. Empty = topology metrics disabled.
	zoneAmbientMetricTemplate string // e.g. datacenter_ambient_temperature_celsius{zone="%s"}
	rackPowerMetricTemplate   string // e.g. datacenter_rack_power_watts{rack="%s"}
	// Known zones and racks to query. Populated by the operator from node labels.
	knownZones []string
	knownRacks []string
}

func defaultFacilityConfig() facilityConfig {
	return facilityConfig{
		enabled:            false,
		prometheusAddress:  "http://prometheus-operated.monitoring:9090",
		pollInterval:       30 * time.Second,
		ambientTempMetric:  "datacenter_ambient_temperature_celsius",
		itPowerMetric:      "datacenter_total_it_power_watts",
		coolingPowerMetric: "datacenter_cooling_power_watts",
	}
}

// facilityMetricsLoop polls Prometheus for data-center-level metrics.
//
// These metrics enable the twin to compute PUE from real facility data
// rather than heuristic estimates. The scheduler then uses PUE to weight
// marginal power costs: a pod on a node with PUE 1.8 costs more than
// one with PUE 1.2.
//
// Supported metrics (configurable via env vars):
//   - datacenter_ambient_temperature_celsius: outside air temp
//   - datacenter_total_it_power_watts: sum of all IT equipment power
//   - datacenter_cooling_power_watts: cooling infrastructure power
//
// PUE = (IT power + cooling power) / IT power
//
// References:
//   - Dayarathna et al. (2016). "Data Center Energy Consumption Modeling: A Survey"
//   - Kaup et al. (2014). "Measuring and Modeling the Power Consumption of OpenFlow Switches"
//   - The Green Grid. "PUE: A Comprehensive Examination of the Metric"
func facilityMetricsLoop(ctx context.Context, fm *facilityMetrics, cfg facilityConfig) {
	if !cfg.enabled {
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	log.Printf("[facility] started: interval=%s prometheus=%s", cfg.pollInterval, cfg.prometheusAddress)

	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetchFacilityMetrics(ctx, client, fm, cfg)
		}
	}
}

func fetchFacilityMetrics(ctx context.Context, client *http.Client, fm *facilityMetrics, cfg facilityConfig) {
	ambientC, err := queryPrometheusScalar(ctx, client, cfg.prometheusAddress, cfg.ambientTempMetric)
	if err != nil {
		log.Printf("[facility] ambient temp query failed: %v", err)
		ambientC = 0
	}

	itPowerW, err := queryPrometheusScalar(ctx, client, cfg.prometheusAddress, cfg.itPowerMetric)
	if err != nil {
		log.Printf("[facility] IT power query failed: %v", err)
		itPowerW = 0
	}

	coolingW, err := queryPrometheusScalar(ctx, client, cfg.prometheusAddress, cfg.coolingPowerMetric)
	if err != nil {
		log.Printf("[facility] cooling power query failed: %v", err)
		coolingW = 0
	}

	fm.set(ambientC, itPowerW, coolingW)

	if itPowerW > 0 {
		pue := (itPowerW + coolingW) / itPowerW
		log.Printf("[facility] ambient=%.1fC itPower=%.0fW cooling=%.0fW pue=%.3f",
			ambientC, itPowerW, coolingW, pue)
	}

	// Per-zone ambient temperature
	if cfg.zoneAmbientMetricTemplate != "" && len(cfg.knownZones) > 0 {
		zoneTemps := make(map[string]float64, len(cfg.knownZones))
		for _, zone := range cfg.knownZones {
			query := fmt.Sprintf(cfg.zoneAmbientMetricTemplate, zone)
			v, err := queryPrometheusScalar(ctx, client, cfg.prometheusAddress, query)
			if err == nil && v > 0 {
				zoneTemps[zone] = v
			}
		}
		// Per-rack power
		rackPower := make(map[string]float64, len(cfg.knownRacks))
		if cfg.rackPowerMetricTemplate != "" {
			for _, rack := range cfg.knownRacks {
				query := fmt.Sprintf(cfg.rackPowerMetricTemplate, rack)
				v, err := queryPrometheusScalar(ctx, client, cfg.prometheusAddress, query)
				if err == nil && v > 0 {
					rackPower[rack] = v
				}
			}
		}
		fm.setTopology(zoneTemps, rackPower)
		if len(zoneTemps) > 0 || len(rackPower) > 0 {
			log.Printf("[facility] topology: zones=%d racks=%d", len(zoneTemps), len(rackPower))
		}
	}
}

// queryPrometheusScalar executes a PromQL instant query and returns the scalar result.
func queryPrometheusScalar(ctx context.Context, client *http.Client, address, query string) (float64, error) {
	u := fmt.Sprintf("%s/api/v1/query?query=%s", address, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []interface{} `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}
	if result.Status != "success" || len(result.Data.Result) == 0 {
		return 0, fmt.Errorf("no data for query %q", query)
	}
	if len(result.Data.Result[0].Value) < 2 {
		return 0, fmt.Errorf("unexpected value format for %q", query)
	}
	valStr, ok := result.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("value not a string for %q", query)
	}
	return strconv.ParseFloat(valStr, 64)
}
