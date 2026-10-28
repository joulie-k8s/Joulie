package main

// Compatibility shims of the OpenTelemetry inputs: inputEnvAliases, the
// legacy facility preset and the legacy HTTP JSON body, added in v0.3.0 and
// removed in inputAliasesRemovedIn. Delete the aliases, lookups and docs rows
// together.

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

// inputEnvAliases maps a facility query variable to the FACILITY_*_METRIC
// name it replaced. The old value is a bare metric name, which the facility
// reader aggregates (avg for a temperature, sum for a power), so it keeps
// meaning what it did.
var inputEnvAliases = map[string]string{
	"FACILITY_AMBIENT_TEMP_QUERY":  "FACILITY_AMBIENT_TEMP_METRIC",
	"FACILITY_IT_POWER_QUERY":      "FACILITY_IT_POWER_METRIC",
	"FACILITY_COOLING_POWER_QUERY": "FACILITY_COOLING_POWER_METRIC",
}

// inputAliasesRemovedIn is when inputEnvAliases, FACILITY_PRESET=legacy and
// the legacy HTTP JSON body go: otlpjson.LegacyRemovedIn, the release every
// component names (TestInputAliasesGoWithTheLegacyFormats). It is spelled
// as a literal because tests/docs reads it from this file to check the
// configuration reference.
const inputAliasesRemovedIn = "v0.4.0"

var (
	deprecatedEnvWarnMu   sync.Mutex
	deprecatedEnvWarnSeen = map[string]struct{}{}
)

// envOrDeprecated is envOrDefault for a variable listed in inputEnvAliases:
// the current name wins, the old name is used when the
// current one is unset, and def applies when neither is set. Reading the old
// name logs one warning per variable naming the replacement and the release
// that removes the old name.
func envOrDeprecated(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	old, removedIn, ok := deprecatedEnvAlias(key)
	if !ok {
		return def
	}
	v := strings.TrimSpace(os.Getenv(old))
	if v == "" {
		return def
	}
	warnDeprecatedOnce(old, fmt.Sprintf("%s is deprecated and will be removed in %s; set %s instead", old, removedIn, key))
	return v
}

// deprecatedEnvAlias returns the old name of key and the release that
// removes it.
func deprecatedEnvAlias(key string) (old, removedIn string, ok bool) {
	if old, ok := inputEnvAliases[key]; ok {
		return old, inputAliasesRemovedIn, true
	}
	return "", "", false
}

// warnDeprecatedOnce logs "warning: " + msg the first time it sees key.
func warnDeprecatedOnce(key, msg string) {
	deprecatedEnvWarnMu.Lock()
	_, seen := deprecatedEnvWarnSeen[key]
	deprecatedEnvWarnSeen[key] = struct{}{}
	deprecatedEnvWarnMu.Unlock()
	if !seen {
		log.Printf("warning: %s", msg)
	}
}
