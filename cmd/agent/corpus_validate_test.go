package main

// Format and privacy validation for the hardware fixture corpus.
//
// cmd/agent/testdata/hardware is published as part of this repository, so a capture that
// still carries a hostname, a MAC address or a serial number is a leak, not a
// review comment. This file is the machine that catches that before a human
// reads the diff, and the same run also checks that the capture has the shape
// the corpus test and the agent expect.
//
// Every check here answers one question a reviewer would otherwise have to ask
// by hand, and every failure message says what to change. The privacy patterns
// are deliberately the ones scripts/collect-hardware-fixture.sh redacts with, so
// the capture and the lint cannot drift apart: whatever the script rewrites,
// this rejects if it survived.
//
// The corpus root comes from the JOULIE_HARDWARE_CORPUS environment variable
// and defaults to this repository's cmd/agent/testdata/hardware. That indirection is here
// so the corpus can later move to a repository of its own, or be validated from
// a checkout somewhere else, without rewriting this test.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/matbun/joulie/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// corpusRootEnv names the environment variable that points at the corpus. When
// it is unset the corpus is the one in this repository.
const corpusRootEnv = "JOULIE_HARDWARE_CORPUS"

// corpusRoot resolves the directory that holds one subdirectory per machine.
func corpusRoot() string {
	if v := strings.TrimSpace(os.Getenv(corpusRootEnv)); v != "" {
		return v
	}
	// corpusDir is "testdata/hardware", relative to this package.
	return corpusDir
}

// ---------------------------------------------------------------------------
// What a capture directory may contain
// ---------------------------------------------------------------------------

// corpusRequiredFiles must exist and be non empty in every machine.
var corpusRequiredFiles = []string{
	"machine.yaml",
	"cpuinfo",
	"node-labels.json",
	"expected.json",
}

// corpusOptionalFiles may exist. Anything not listed here and not required is
// rejected, so a forgotten tarball or an editor backup fails the build.
var corpusOptionalFiles = map[string]string{
	"nvidia-smi.txt": "stdout of the agent's NVIDIA query, absent when nvidia-smi is not installed",
	"rocm-smi.txt":   "stdout of the agent's ROCm query, absent when rocm-smi is not installed",
	"cpufreq-driver": "the active cpufreq scaling driver, one word",
	"README.md":      "notes that do not fit in machine.yaml",
}

// corpusPowercapDir is the only subdirectory a machine may have.
const corpusPowercapDir = "powercap"

// corpusMachineYAMLKeys are the top level keys machine.yaml may carry, and
// whether each one is required.
var corpusMachineYAMLKeys = map[string]bool{
	"description":         true,
	"source":              true,
	"capturedAt":          true,
	"allocatable":         true,
	"writeRejectingZones": true,
	"notes":               false,
}

// corpusZoneDirPattern is how sysfs names a powercap zone: intel-rapl:0 for a
// package zone, intel-rapl:0:0 for one of its sub-zones.
var corpusZoneDirPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*:[0-9]+(:[0-9]+)?$`)

// corpusZoneFilePattern lists the attributes the capture script copies out of a
// zone. Anything else in a zone directory did not come from the script.
var corpusZoneFilePattern = regexp.MustCompile(
	`^(name|enabled|energy_uj|max_energy_range_uj|constraint_[0-9]+_(name|power_limit_uw|max_power_uw|min_power_uw|time_window_us))$`)

// corpusNvidiaQuery and corpusRocmQuery are the queries whose stdout the
// fixtures hold. TestCorpusGPUQueriesMatchTheAgent asserts that cmd/agent still
// runs exactly these, so the fixture format cannot drift away from the agent.
const (
	corpusNvidiaQuery = "--query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name"
	corpusRocmQuery   = `"rocm-smi", "--showpowercap", "--showproductname", "--json"`
)

// corpusNvidiaColumns is the number of fields that query returns per GPU:
// index, power.min_limit, power.max_limit, power.limit, power.draw, name.
const corpusNvidiaColumns = 6

// ---------------------------------------------------------------------------
// Privacy patterns
//
// These mirror the sed program in scripts/collect-hardware-fixture.sh. A match is
// ignored when the matched text contains REDACTED, because that is what the
// script writes in place of the real value.
// ---------------------------------------------------------------------------

type corpusPrivacyPattern struct {
	what string
	re   *regexp.Regexp
	fix  string
}

var corpusPrivacyPatterns = []corpusPrivacyPattern{
	{
		what: "a MAC address",
		re:   regexp.MustCompile(`(?i)\b[0-9a-f]{2}([:-][0-9a-f]{2}){5}\b`),
		fix:  "replace it with MAC-REDACTED, or drop the file: the corpus never needs a network address",
	},
	{
		what: "an IPv4 address",
		re:   regexp.MustCompile(`\b([0-9]{1,3}\.){3}[0-9]{1,3}(/[0-9]{1,2})?\b`),
		fix:  "replace it with IPV4-REDACTED",
	},
	{
		what: "an IPv6 address",
		re:   regexp.MustCompile(`(?i)\b([0-9a-f]{1,4}:){5,7}[0-9a-f]{1,4}\b`),
		fix:  "replace it with IPV6-REDACTED",
	},
	{
		what: "an IPv6 address",
		re:   regexp.MustCompile(`(?i)\b[0-9a-f]{1,4}(:[0-9a-f]{1,4}){0,4}::([0-9a-f]{1,4}(:[0-9a-f]{1,4}){0,4})?\b`),
		fix:  "replace it with IPV6-REDACTED",
	},
	{
		what: "a UUID",
		re:   regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`),
		fix:  "replace it with UUID-REDACTED: a GPU or system UUID identifies the machine forever",
	},
	{
		what: "a GPU or MIG UUID",
		re:   regexp.MustCompile(`\b(GPU|MIG)-[0-9a-fA-F][0-9a-fA-F-]{15,}\b`),
		fix:  "replace it with GPU-REDACTED or MIG-REDACTED",
	},
	{
		what: "a serial number or an asset tag",
		re:   regexp.MustCompile(`(?i)\b(serial(\s*[_-]?\s*number)?|asset\s*[_-]?\s*tag)\s*[:=]\s*[A-Za-z0-9][A-Za-z0-9-]{3,}`),
		fix:  "replace the value with SERIAL-REDACTED or ASSET-REDACTED",
	},
	{
		what: "a home directory path, which carries a username",
		re:   regexp.MustCompile(`(?i)/(home|users|export/home)/[a-z0-9._-]+`),
		fix:  "remove the path: nothing the agent reads lives under a home directory",
	},
	{
		what: "a username",
		re:   regexp.MustCompile(`(?i)\b(user|username|login|logname)\s*[:=]\s*[a-z][a-z0-9._-]{2,}`),
		fix:  "remove the line: the corpus describes hardware, not who was logged in",
	},
	{
		what: "a cloud instance id",
		re:   regexp.MustCompile(`\bi-[0-9a-f]{8,17}\b`),
		fix:  "remove it: an instance id names one machine in one account",
	},
	{
		what: "a cloud instance id",
		re:   regexp.MustCompile(`(?i)\binstance[-_ ]?id\s*[:=]\s*\S+`),
		fix:  "remove it: an instance id names one machine in one account",
	},
	{
		what: "a cloud provider id",
		re:   regexp.MustCompile(`(?i)\b(aws|gce|gcp|azure|openstack|vsphere)://\S+`),
		fix:  "remove it: a provider id names one machine in one account",
	},
}

// corpusHostnameRe finds dotted names. It is filtered afterwards by
// corpusKnownDomains and corpusHostnameTLDs, because label keys such as
// feature.node.kubernetes.io/cpu-model.name are dotted too and must survive.
var corpusHostnameRe = regexp.MustCompile(`(?i)\b[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+\b`)

// corpusKnownDomains are suffixes that belong to Kubernetes and vendor
// identifiers, not to the contributor's site.
var corpusKnownDomains = []string{
	"kubernetes.io", "k8s.io", "sigs.k8s.io", "joulie.io",
	"nvidia.com", "amd.com", "intel.com",
	"example.com", "example.org", "example.net",
	"github.com", "gcr.io", "ghcr.io", "docker.io", "quay.io",
	"openshift.io", "cri-o.io", "kubevirt.io", "golang.org", "apache.org",
}

// corpusHostnameTLDs is the last label of something that is probably a machine
// or a site, rather than a dotted identifier. Kept deliberately short: a false
// positive here blocks a contribution, so the suffixes are ones that do not
// occur as the tail of a Kubernetes label, a version string or a file name.
var corpusHostnameTLDs = map[string]bool{
	"com": true, "net": true, "org": true, "io": true, "edu": true,
	"gov": true, "mil": true, "int": true, "eu": true, "us": true,
	"uk": true, "de": true, "fr": true, "es": true, "ch": true,
	"nl": true, "se": true, "dk": true, "pl": true, "cz": true,
	"at": true, "be": true, "ca": true, "au": true, "jp": true,
	"cn": true, "br": true, "local": true, "internal": true,
	"intranet": true, "lan": true, "corp": true, "cloud": true,
	"private": true, "arpa": true,
}

// ---------------------------------------------------------------------------
// The validator
// ---------------------------------------------------------------------------

// corpusProblems collects failures, each already carrying the file and the line
// the contributor has to open.
type corpusProblems struct {
	machine string
	msgs    []string
}

func (p *corpusProblems) addf(file string, line int, format string, args ...any) {
	loc := path.Join(p.machine, file)
	if line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, line)
	}
	p.msgs = append(p.msgs, loc+": "+fmt.Sprintf(format, args...))
}

// validateCorpusMachine checks one capture directory and returns one message
// per problem. An empty result means the capture is publishable as is.
func validateCorpusMachine(root, machine string) []string {
	p := &corpusProblems{machine: machine}
	dir := filepath.Join(root, machine)

	present := validateCorpusLayout(p, dir)
	validateCorpusMachineYAML(p, dir)
	validateCorpusCPUInfo(p, dir)
	validateCorpusPowercap(p, dir)
	if present["nvidia-smi.txt"] {
		validateCorpusNvidiaOutput(p, dir)
	}
	if present["rocm-smi.txt"] {
		validateCorpusRocmOutput(p, dir)
	}
	validateCorpusNodeLabels(p, dir)
	validateCorpusGolden(p, dir)
	validateCorpusPrivacy(p, dir)
	return p.msgs
}

// validateCorpusLayout checks that every required file is there and non empty,
// that every optional file is one we recognise, and that nothing else is.
func validateCorpusLayout(p *corpusProblems, dir string) map[string]bool {
	present := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		p.addf(".", 0, "cannot be read: %v. Fix: the capture must be a directory under the corpus root", err)
		return present
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name != corpusPowercapDir {
				p.addf(name, 0, "unexpected directory. Fix: %q is the only subdirectory a capture may have; delete this one", corpusPowercapDir)
			}
			present[name] = true
			continue
		}
		present[name] = true
		if _, ok := corpusOptionalFiles[name]; ok {
			continue
		}
		if corpusContains(corpusRequiredFiles, name) {
			continue
		}
		p.addf(name, 0, "unexpected file. Fix: delete it. A capture may contain only %s, "+
			"the optional files %s, and the %s/ directory",
			strings.Join(corpusRequiredFiles, ", "), strings.Join(corpusSortedKeys(corpusOptionalFiles), ", "), corpusPowercapDir)
	}
	for _, name := range corpusRequiredFiles {
		if !present[name] {
			p.addf(name, 0, "required file is missing. Fix: copy it from cmd/agent/testdata/hardware/_template and fill it in, "+
				"or rerun scripts/collect-hardware-fixture.sh")
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			p.addf(name, 0, "cannot be read: %v", err)
			continue
		}
		if len(bytes.TrimSpace(b)) == 0 {
			p.addf(name, 0, "is empty. Fix: a required file with no content proves nothing; fill it in or remove the machine")
		}
	}
	return present
}

// validateCorpusMachineYAML checks the hand written half of the capture.
func validateCorpusMachineYAML(p *corpusProblems, dir string) {
	const file = "machine.yaml"
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return // already reported by the layout check
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		p.addf(file, 0, "does not parse as YAML: %v. Fix: compare it with cmd/agent/testdata/hardware/_template/machine.yaml", err)
		return
	}
	unknown := []string{}
	for key := range raw {
		if _, known := corpusMachineYAMLKeys[key]; !known {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		p.addf(file, 0, "unknown key %q. Fix: remove it. The accepted keys are %s",
			key, strings.Join(corpusSortedBoolKeys(corpusMachineYAMLKeys), ", "))
	}
	// A key that is absent, and a key whose value is an explicit null, are the
	// same omission from the reader's side.
	for _, key := range corpusSortedBoolKeys(corpusMachineYAMLKeys) {
		if !corpusMachineYAMLKeys[key] {
			continue
		}
		if v, set := raw[key]; !set || v == nil {
			p.addf(file, 0, "missing required key %q. Fix: copy it from cmd/agent/testdata/hardware/_template/machine.yaml, "+
				"which documents what goes in it", key)
		}
	}

	var meta corpusMachine
	if err := yaml.Unmarshal(b, &meta); err != nil {
		p.addf(file, 0, "does not decode into the corpus metadata: %v", err)
		return
	}
	for _, key := range []string{"description", "source"} {
		value := meta.Description
		if key == "source" {
			value = meta.Source
		}
		if v, set := raw[key]; !set || v == nil {
			continue // already reported as missing
		}
		if strings.TrimSpace(value) == "" {
			p.addf(file, 0, "%s is empty. Fix: write it. %s is what tells the next maintainer whether this "+
				"machine may be deleted", key, key)
			continue
		}
		if strings.Contains(value, "TODO") {
			p.addf(file, 0, "%s still says TODO. Fix: write it. %s is what tells the next maintainer "+
				"whether this machine may be deleted", key, key)
		}
	}
	if _, set := raw["capturedAt"]; set {
		if !corpusParsesAsDate(meta.CapturedAt) {
			p.addf(file, 0, "capturedAt is %q, which is not a date. Fix: use an RFC3339 timestamp such as "+
				"2026-01-31T09:00:00Z, or a plain 2026-01-31", meta.CapturedAt)
		}
	}
	if meta.Allocatable.CPU == "" || strings.Contains(meta.Allocatable.CPU, "TODO") {
		p.addf(file, 0, "allocatable.cpu is %q. Fix: put what the kubelet reports, from "+
			"kubectl get node <node> -o jsonpath='{.status.allocatable.cpu}'", meta.Allocatable.CPU)
	} else if _, err := resource.ParseQuantity(meta.Allocatable.CPU); err != nil {
		p.addf(file, 0, "allocatable.cpu %q is not a Kubernetes quantity: %v. Fix: use the kubelet's own spelling, for example \"192\"",
			meta.Allocatable.CPU, err)
	}
	if meta.Allocatable.Memory == "" || strings.Contains(meta.Allocatable.Memory, "TODO") {
		p.addf(file, 0, "allocatable.memory is %q. Fix: put what the kubelet reports, for example \"394993664Ki\"",
			meta.Allocatable.Memory)
	} else if _, err := resource.ParseQuantity(meta.Allocatable.Memory); err != nil {
		p.addf(file, 0, "allocatable.memory %q is not a Kubernetes quantity: %v. Fix: use the kubelet's own spelling, for example \"394993664Ki\"",
			meta.Allocatable.Memory, err)
	}
	for _, name := range corpusSortedKeys(meta.Allocatable.GPU) {
		qty := meta.Allocatable.GPU[name]
		if !strings.Contains(name, "/") {
			p.addf(file, 0, "allocatable.gpu key %q is not a Kubernetes resource name. Fix: use the name the device "+
				"plugin advertises, such as nvidia.com/gpu", name)
		}
		if _, err := resource.ParseQuantity(qty); err != nil {
			p.addf(file, 0, "allocatable.gpu[%q] is %q, not a Kubernetes quantity: %v. Fix: quote a whole number, such as \"8\"",
				name, qty, err)
		}
	}
	for _, zone := range meta.WriteRejectingZones {
		limit := filepath.Join(dir, corpusPowercapDir, zone, "constraint_0_power_limit_uw")
		if _, err := os.Stat(limit); err != nil {
			p.addf(file, 0, "writeRejectingZones names %q, but %s/%s/constraint_0_power_limit_uw does not exist. "+
				"Fix: name a zone directory that is in this capture, or drop the entry",
				zone, corpusPowercapDir, zone)
		}
	}
}

// validateCorpusCPUInfo checks that the captured /proc/cpuinfo carries the two
// facts readProcCPUInfo reads, and that it was not truncated mid block.
func validateCorpusCPUInfo(p *corpusProblems, dir string) {
	const file = "cpuinfo"
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return // already reported by the layout check
	}
	processors, physicalIDs := 0, 0
	distinct := map[string]bool{}
	models := 0
	for i, line := range strings.Split(string(b), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "processor":
			processors++
			if _, err := strconv.Atoi(value); err != nil {
				p.addf(file, i+1, "processor index %q is not a number. Fix: do not hand edit cpuinfo; copy it with "+
					"scripts/collect-hardware-fixture.sh", value)
			}
		case "model name":
			if value != "" {
				models++
			}
		case "physical id":
			physicalIDs++
			distinct[value] = true
			if n, err := strconv.Atoi(value); err != nil || n < 0 {
				p.addf(file, i+1, "physical id %q is not a non negative number. Fix: do not hand edit cpuinfo", value)
			}
		}
	}
	if processors == 0 {
		p.addf(file, 0, "has no \"processor\" block. Fix: this is not a /proc/cpuinfo. Recapture with "+
			"scripts/collect-hardware-fixture.sh")
		return
	}
	if models == 0 {
		p.addf(file, 0, "has no non empty \"model name\" line. Fix: without it the hardware catalog can never match "+
			"and the node has no TDP; recapture on a machine that reports one")
	}
	if physicalIDs > 0 {
		if physicalIDs != processors {
			p.addf(file, 0, "has %d \"processor\" blocks but %d \"physical id\" lines. Fix: keep one physical id per "+
				"processor block; a trimmed cpuinfo must be trimmed whole blocks at a time",
				processors, physicalIDs)
		}
		if len(distinct) > processors {
			p.addf(file, 0, "has %d distinct \"physical id\" values across %d processor blocks. Fix: a socket cannot "+
				"have fewer than one logical CPU; do not hand edit cpuinfo", len(distinct), processors)
		}
	}
}

// validateCorpusPowercap checks the captured sysfs tree, when there is one.
func validateCorpusPowercap(p *corpusProblems, dir string) {
	root := filepath.Join(dir, corpusPowercapDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return // no RAPL on this machine, which is a valid capture
	}
	zones := 0
	for _, e := range entries {
		name := e.Name()
		rel := path.Join(corpusPowercapDir, name)
		if !e.IsDir() {
			p.addf(rel, 0, "is a file. Fix: %s/ holds one directory per zone and nothing else",
				corpusPowercapDir)
			continue
		}
		zones++
		if !corpusZoneDirPattern.MatchString(name) {
			p.addf(rel, 0, "is not a powercap zone name. Fix: keep the sysfs directory names, such as "+
				"intel-rapl:0 for a package zone and intel-rapl:0:0 for one of its sub-zones")
		}
		zoneFiles, err := os.ReadDir(filepath.Join(root, name))
		if err != nil {
			p.addf(rel, 0, "cannot be read: %v", err)
			continue
		}
		hasName, hasConstraint0 := false, false
		for _, zf := range zoneFiles {
			if zf.IsDir() {
				p.addf(path.Join(rel, zf.Name()), 0, "is a directory. Fix: a zone holds plain attribute files only")
				continue
			}
			if !corpusZoneFilePattern.MatchString(zf.Name()) {
				p.addf(path.Join(rel, zf.Name()), 0, "unexpected file in a powercap zone. Fix: delete it; "+
					"scripts/collect-hardware-fixture.sh copies name, enabled, energy_uj, max_energy_range_uj and the constraint_N_* attributes")
				continue
			}
			if zf.Name() == "name" {
				b, err := os.ReadFile(filepath.Join(root, name, "name"))
				if err == nil && len(bytes.TrimSpace(b)) > 0 {
					hasName = true
				}
			}
			if strings.HasPrefix(zf.Name(), "constraint_0_") {
				hasConstraint0 = true
			}
		}
		if !hasName {
			p.addf(rel, 0, "has no non empty \"name\" file. Fix: capture it. The agent selects package zones by that "+
				"file and never by the directory pattern, so a zone without it is invisible")
		}
		if !hasConstraint0 {
			p.addf(rel, 0, "has no constraint_0_* file. Fix: capture at least constraint_0_max_power_uw and "+
				"constraint_0_power_limit_uw; without them the zone carries no cap range")
		}
	}
	if zones == 0 {
		p.addf(corpusPowercapDir, 0, "is empty. Fix: delete the directory. A machine with no RAPL is a valid "+
			"capture, an empty powercap tree is not")
	}
}

// validateCorpusNvidiaOutput checks that the captured stdout is the CSV the
// agent's query produces, field for field.
func validateCorpusNvidiaOutput(p *corpusProblems, dir string) {
	const file = "nvidia-smi.txt"
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return
	}
	rows := 0
	for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rows++
		// splitCSVLine is the agent's own splitter, so the fixture is read
		// exactly the way cmd/agent reads it.
		parts := splitCSVLine(line)
		if len(parts) < corpusNvidiaColumns {
			p.addf(file, i+1, "has %d comma separated fields, want %d. Fix: capture the exact query the agent runs:\n"+
				"  nvidia-smi %s --format=csv,noheader,nounits",
				len(parts), corpusNvidiaColumns, corpusNvidiaQuery)
			continue
		}
		if _, err := strconv.Atoi(parts[0]); err != nil {
			p.addf(file, i+1, "field 1 (index) is %q, not a number. Fix: capture with --format=csv,noheader,nounits "+
				"so there is no header row and no unit suffix", parts[0])
		}
		for col, what := range []string{"power.min_limit", "power.max_limit", "power.limit", "power.draw"} {
			v := parts[col+1]
			if corpusIsNotApplicable(v) {
				continue
			}
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				p.addf(file, i+1, "field %d (%s) is %q, not a number and not [N/A]. Fix: capture with nounits, "+
					"which drops the \" W\" suffix", col+2, what, v)
			}
		}
		if strings.TrimSpace(parts[5]) == "" {
			p.addf(file, i+1, "field 6 (name) is empty. Fix: the GPU product name is what the hardware catalog is "+
				"matched against; recapture")
		}
	}
	if rows == 0 {
		p.addf(file, 0, "has no rows. Fix: delete it. A machine without a working nvidia-smi has no nvidia-smi.txt")
	}
}

// validateCorpusRocmOutput checks the captured ROCm JSON.
func validateCorpusRocmOutput(p *corpusProblems, dir string) {
	const file = "rocm-smi.txt"
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		p.addf(file, 0, "does not parse as a JSON object: %v. Fix: capture with "+
			"rocm-smi --showpowercap --showproductname --json", err)
		return
	}
	cards := 0
	for k, v := range raw {
		if !strings.HasPrefix(strings.ToLower(k), "card") {
			continue
		}
		cards++
		var obj map[string]any
		if err := json.Unmarshal(v, &obj); err != nil {
			p.addf(file, 0, "key %q is not an object: %v. Fix: capture the unmodified --json output", k, err)
		}
	}
	if cards == 0 {
		p.addf(file, 0, "has no \"card<N>\" key, so the agent would see no GPU. Fix: capture with "+
			"rocm-smi --showpowercap --showproductname --json, or delete the file if the node has no AMD GPU")
	}
}

// validateCorpusNodeLabels checks that the labels are a flat object of strings
// and that the hostname was redacted.
func validateCorpusNodeLabels(p *corpusProblems, dir string) {
	const file = "node-labels.json"
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		p.addf(file, 0, "is not a flat JSON object of strings: %v. Fix: capture it with "+
			"kubectl get node <node> -o jsonpath='{.metadata.labels}', or write {} if the machine is not a node", err)
		return
	}
	for _, k := range corpusSortedRawKeys(raw) {
		var s string
		if err := json.Unmarshal(raw[k], &s); err != nil {
			p.addf(file, 0, "is not a flat JSON object of strings: the value of %q is %s. Fix: node labels are "+
				"always strings; quote it", k, string(raw[k]))
			continue
		}
		if k == "kubernetes.io/hostname" && s != "REDACTED-HOST" && s != "" {
			p.addf(file, 0, "kubernetes.io/hostname is %q. Fix: replace it with REDACTED-HOST, which is what "+
				"scripts/collect-hardware-fixture.sh writes in place of every spelling of the machine's name", s)
		}
	}
}

// validateCorpusGolden checks that expected.json is what the status writer
// produces, and that nobody hand edited a field into it.
func validateCorpusGolden(p *corpusProblems, dir string) {
	const file = "expected.json"
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var status v1alpha1.NodeHardwareStatus
	if err := dec.Decode(&status); err != nil {
		p.addf(file, 0, "does not parse as a NodeHardware status: %v. Fix: never hand edit it; regenerate with "+
			"go test ./cmd/agent/ -run Corpus -update", err)
		return
	}
	if status.UpdatedAt != "" {
		p.addf(file, 0, "carries updatedAt, which the golden writer strips because it is wall clock, not a "+
			"hardware fact. Fix: regenerate with go test ./cmd/agent/ -run Corpus -update")
	}
}

// validateCorpusPrivacy scans every file of the capture. The corpus is
// published with this repository, so this is the last gate before a hostname,
// an address or a serial number becomes permanent.
func validateCorpusPrivacy(p *corpusProblems, dir string) {
	_ = filepath.WalkDir(dir, func(fp string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, fp)
		if relErr != nil {
			rel = fp
		}
		rel = filepath.ToSlash(rel)
		b, readErr := os.ReadFile(fp)
		if readErr != nil {
			return nil
		}
		if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
			p.addf(rel, 0, "is not a text file. Fix: a capture holds text only; delete it")
			return nil
		}
		// One offending string gets one message, from the first pattern that
		// names it: a MAC address also has the shape of an IPv6 address, and
		// two messages about the same characters help nobody.
		reported := map[string]bool{}
		for i, line := range strings.Split(string(b), "\n") {
			for _, pat := range corpusPrivacyPatterns {
				for _, m := range pat.re.FindAllString(line, -1) {
					if strings.Contains(strings.ToUpper(m), "REDACTED") || reported[fmt.Sprint(i, m)] {
						continue
					}
					reported[fmt.Sprint(i, m)] = true
					p.addf(rel, i+1, "contains %s: %q. Fix: %s", pat.what, corpusTruncate(m), pat.fix)
				}
			}
			for _, m := range corpusHostnameRe.FindAllString(line, -1) {
				if !corpusLooksLikeHostname(m) || reported[fmt.Sprint(i, m)] {
					continue
				}
				reported[fmt.Sprint(i, m)] = true
				p.addf(rel, i+1, "contains a hostname or FQDN: %q. Fix: replace it with REDACTED-HOST. "+
					"scripts/collect-hardware-fixture.sh does that for every spelling of the machine's name, "+
					"including the domain on its own", corpusTruncate(m))
			}
		}
		return nil
	})
}

// corpusLooksLikeHostname decides whether a dotted token names a machine or a
// site rather than a Kubernetes label, a version or a file. It must not fire on
// feature.node.kubernetes.io/cpu-model.name, 535.183.01 or main.go.
func corpusLooksLikeHostname(token string) bool {
	lower := strings.ToLower(token)
	if strings.Contains(strings.ToUpper(token), "REDACTED") {
		return false
	}
	labels := strings.Split(lower, ".")
	if len(labels) < 2 {
		return false
	}
	if !corpusHostnameTLDs[labels[len(labels)-1]] {
		return false
	}
	for _, known := range corpusKnownDomains {
		if lower == known || strings.HasSuffix(lower, "."+known) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func corpusParsesAsDate(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01-02T15:04:05"} {
		if _, err := time.Parse(layout, v); err == nil {
			return true
		}
	}
	return false
}

func corpusIsNotApplicable(v string) bool {
	v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), "[]"))
	return strings.EqualFold(v, "N/A") || strings.EqualFold(v, "Not Supported")
}

func corpusTruncate(s string) string {
	const max = 48
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func corpusContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func corpusSortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func corpusSortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func corpusSortedRawKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// corpusMachineDirs lists the machines in a corpus root. Directories whose name
// starts with an underscore are not machines: _template is the contributor's
// starting point and is deliberately full of placeholders.
func corpusMachineDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestHardwareCorpusIsValid runs the validator over every machine that is
// actually in the corpus. It is the check that runs in plain go test ./... and
// that a contributor runs before opening a pull request.
func TestHardwareCorpusIsValid(t *testing.T) {
	root := corpusRoot()
	machines, err := corpusMachineDirs(root)
	if err != nil {
		t.Fatalf("cannot read the corpus at %s: %v (set %s to point elsewhere)", root, err, corpusRootEnv)
	}
	if len(machines) == 0 {
		t.Fatalf("no machines in %s", root)
	}
	for _, machine := range machines {
		t.Run(machine, func(t *testing.T) {
			for _, msg := range validateCorpusMachine(root, machine) {
				t.Errorf("%s", msg)
			}
		})
	}
}

// TestCorpusListingsSkipUnderscoreDirectories pins the rule that both listings
// ignore _template, rather than leaving it to be noticed when the corpus test
// tries to replay a directory full of placeholders.
func TestCorpusListingsSkipUnderscoreDirectories(t *testing.T) {
	// The template has to exist, otherwise this test would pass vacuously.
	if _, err := os.Stat(filepath.Join(corpusRoot(), "_template", "machine.yaml")); err != nil {
		t.Fatalf("cmd/agent/testdata/hardware/_template is missing: %v", err)
	}
	for _, machine := range corpusMachines(t) {
		if strings.HasPrefix(machine, "_") {
			t.Errorf("corpusMachines returned %q; the corpus test must skip underscore directories", machine)
		}
	}

	root := t.TempDir()
	for _, name := range []string{"_template", "_scratch", "real-machine"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := corpusMachineDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "real-machine" {
		t.Fatalf("corpusMachineDirs = %v, want [real-machine]", got)
	}
}

// TestCorpusGPUQueriesMatchTheAgent keeps the fixture format and the agent from
// drifting apart: the CSV this file validates is only meaningful while
// cmd/agent still asks for exactly those columns.
func TestCorpusGPUQueriesMatchTheAgent(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, corpusNvidiaQuery) {
		t.Errorf("cmd/agent/main.go no longer runs %q; update corpusNvidiaQuery and corpusNvidiaColumns, "+
			"and recapture or re-review every nvidia-smi.txt in the corpus", corpusNvidiaQuery)
	}
	if !strings.Contains(src, corpusRocmQuery) {
		t.Errorf("cmd/agent/main.go no longer runs %s; update corpusRocmQuery and re-review every rocm-smi.txt "+
			"in the corpus", corpusRocmQuery)
	}
}

// ---------------------------------------------------------------------------
// Negative cases
//
// A validator nobody ever saw reject anything is decoration. Each case below
// builds a capture that is good except for one thing, and asserts that the
// validator names it. The good capture itself is the false positive trap: it
// carries a powercap sub-zone named intel-rapl:0:0, an NVIDIA driver version,
// a bogomips value and a CPU model full of numbers, all of which must survive.
// ---------------------------------------------------------------------------

const corpusGoodCPUInfo = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
model name	: Intel(R) Xeon(R) Gold 6252 CPU @ 2.10GHz
stepping	: 7
microcode	: 0x5003604
cpu MHz		: 2100.000
physical id	: 0
siblings	: 2
core id		: 0
cpu cores	: 1
flags		: fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat sse4_1 sse4_2 avx512f
bugs		: spectre_v1 spectre_v2 spec_store_bypass
bogomips	: 4199.85
address sizes	: 46 bits physical, 48 bits virtual

processor	: 1
vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
model name	: Intel(R) Xeon(R) Gold 6252 CPU @ 2.10GHz
stepping	: 7
microcode	: 0x5003604
cpu MHz		: 2100.000
physical id	: 1
siblings	: 2
core id		: 0
cpu cores	: 1
flags		: fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat sse4_1 sse4_2 avx512f
bugs		: spectre_v1 spectre_v2 spec_store_bypass
bogomips	: 4199.85
address sizes	: 46 bits physical, 48 bits virtual
`

// corpusGoodMachineYAML mentions an NVIDIA driver version on purpose: 535.183.01
// is three dotted numbers and must not be read as an IPv4 address.
const corpusGoodMachineYAML = `description: >-
  Two socket Intel Xeon Gold 6252 with one NVIDIA GPU, NVIDIA driver 535.183.01,
  kernel 4.18.0-553.36.1.el8_10.x86_64.
source: >-
  Captured with scripts/collect-hardware-fixture.sh and reviewed file by file.
capturedAt: "2026-01-31T09:00:00Z"
allocatable:
  cpu: "2"
  memory: "16384Ki"
  gpu:
    nvidia.com/gpu: "1"
writeRejectingZones:
  - "intel-rapl:0:0"
`

const corpusGoodNodeLabels = `{
  "beta.kubernetes.io/arch": "amd64",
  "feature.node.kubernetes.io/cpu-model.vendor_id": "Intel",
  "feature.node.kubernetes.io/kernel-version.full": "4.18.0-553.36.1.el8_10.x86_64",
  "feature.node.kubernetes.io/system-os_release.VERSION_ID": "8.10",
  "kubernetes.io/hostname": "REDACTED-HOST",
  "node-role.kubernetes.io/worker": ""
}
`

const corpusGoodNvidiaCSV = "0, 60.00, 70.00, 70.00, 9.97, Tesla T4\n"

const corpusGoodExpected = `{
  "capabilities": {
    "cpuControl": true,
    "cpuTelemetry": true
  },
  "cpu": {
    "rawModel": "Intel(R) Xeon(R) Gold 6252 CPU @ 2.10GHz",
    "sockets": 2
  }
}
`

// newCorpusTestMachine writes a capture that the validator accepts, and returns
// the corpus root and the machine directory.
func newCorpusTestMachine(t *testing.T) (root, machine, dir string) {
	t.Helper()
	root = t.TempDir()
	machine = "good-machine"
	dir = filepath.Join(root, machine)
	corpusWrite(t, dir, "machine.yaml", corpusGoodMachineYAML)
	corpusWrite(t, dir, "cpuinfo", corpusGoodCPUInfo)
	corpusWrite(t, dir, "node-labels.json", corpusGoodNodeLabels)
	corpusWrite(t, dir, "expected.json", corpusGoodExpected)
	corpusWrite(t, dir, "nvidia-smi.txt", corpusGoodNvidiaCSV)
	corpusWrite(t, dir, "cpufreq-driver", "intel_pstate\n")
	// README.md is an accepted optional file, and having one here proves it.
	corpusWrite(t, dir, "README.md", "Notes on this capture.\n")
	// A package zone and the DRAM sub-zone that must not be mistaken for an
	// IPv6 address or for a package.
	for _, z := range []struct{ name, kind, limit, max string }{
		{"intel-rapl:0", "package-0", "165000000", "165000000"},
		{"intel-rapl:0:0", "dram", "0", "47250000"},
	} {
		corpusWrite(t, dir, filepath.Join("powercap", z.name, "name"), z.kind+"\n")
		corpusWrite(t, dir, filepath.Join("powercap", z.name, "enabled"), "1\n")
		corpusWrite(t, dir, filepath.Join("powercap", z.name, "energy_uj"), "10000000000\n")
		corpusWrite(t, dir, filepath.Join("powercap", z.name, "constraint_0_name"), "long_term\n")
		corpusWrite(t, dir, filepath.Join("powercap", z.name, "constraint_0_power_limit_uw"), z.limit+"\n")
		corpusWrite(t, dir, filepath.Join("powercap", z.name, "constraint_0_max_power_uw"), z.max+"\n")
	}
	return root, machine, dir
}

func corpusWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// corpusAppend adds a line to a file of the capture, creating it if it is one
// of the optional files the good capture does not have.
func corpusAppend(t *testing.T, dir, rel, content string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	corpusWrite(t, dir, rel, string(b)+content)
}

// TestCorpusValidatorAcceptsAGoodCapture is the false positive trap: a capture
// with an intel-rapl:0:0 zone, a 535.183.01 driver version, a bogomips value
// and a numeric CPU model passes with nothing to say.
func TestCorpusValidatorAcceptsAGoodCapture(t *testing.T) {
	root, machine, _ := newCorpusTestMachine(t)
	if msgs := validateCorpusMachine(root, machine); len(msgs) != 0 {
		t.Fatalf("a good capture was rejected:\n%s", strings.Join(msgs, "\n"))
	}
}

// TestCorpusValidatorFalsePositiveTraps names each thing that must survive
// individually, so a regression says which one broke.
func TestCorpusValidatorFalsePositiveTraps(t *testing.T) {
	traps := []struct {
		name string
		text string
	}{
		{"powercap sub-zone name", "intel-rapl:0:0"},
		{"NVIDIA driver version", "535.183.01"},
		{"bogomips", "bogomips	: 4199.85"},
		{"CPU model with numbers", "Intel(R) Xeon(R) Gold 6252 CPU @ 2.10GHz"},
		{"CPU flags", "flags		: fpu vme de pse tsc msr sse4_1 avx512f md_clear"},
		{"kernel version label", "4.18.0-553.36.1.el8_10.x86_64"},
		{"NFD label key", "feature.node.kubernetes.io/cpu-model.name"},
		{"cpu MHz", "cpu MHz		: 2099.926"},
		{"microcode", "microcode	: 0x5003604"},
		{"address sizes", "address sizes	: 46 bits physical, 48 bits virtual"},
	}
	for _, trap := range traps {
		t.Run(trap.name, func(t *testing.T) {
			root, machine, dir := newCorpusTestMachine(t)
			corpusAppend(t, dir, "README.md", trap.text+"\n")
			for _, msg := range validateCorpusMachine(root, machine) {
				t.Errorf("%q must not be flagged, but: %s", trap.text, msg)
			}
		})
	}
}

// TestCorpusValidatorRejects covers every class of check. Each case mutates the
// good capture in exactly one way.
func TestCorpusValidatorRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string)
		want   string
	}{
		// --- layout ---
		{
			name:   "missing required file",
			mutate: func(t *testing.T, dir string) { corpusRemove(t, dir, "machine.yaml") },
			want:   "machine.yaml: required file is missing",
		},
		{
			name:   "empty required file",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "cpuinfo", "\n\n") },
			want:   "cpuinfo: is empty",
		},
		{
			name:   "stray tarball",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "capture.tar", "junk") },
			want:   "capture.tar: unexpected file",
		},
		{
			name:   "editor backup",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "machine.yaml~", "junk") },
			want:   "machine.yaml~: unexpected file",
		},
		{
			name: "stray directory",
			mutate: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "sysfs"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "sysfs: unexpected directory",
		},
		// --- machine.yaml ---
		{
			name:   "machine.yaml does not parse",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "machine.yaml", "description: [unclosed\n") },
			want:   "machine.yaml: does not parse as YAML",
		},
		{
			name: "machine.yaml missing a required key",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "machine.yaml",
					strings.Replace(corpusGoodMachineYAML, `capturedAt: "2026-01-31T09:00:00Z"`, "", 1))
			},
			want: `missing required key "capturedAt"`,
		},
		{
			name:   "machine.yaml unknown key",
			mutate: func(t *testing.T, dir string) { corpusAppend(t, dir, "machine.yaml", "datacentre: room 3\n") },
			want:   `unknown key "datacentre"`,
		},
		{
			name: "machine.yaml capturedAt is not a date",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "machine.yaml",
					strings.Replace(corpusGoodMachineYAML, `"2026-01-31T09:00:00Z"`, `"last tuesday"`, 1))
			},
			want: "capturedAt is \"last tuesday\", which is not a date",
		},
		{
			name: "machine.yaml description still says TODO",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "machine.yaml",
					strings.Replace(corpusGoodMachineYAML, "Two socket Intel", "TODO Two socket Intel", 1))
			},
			want: "description still says TODO",
		},
		{
			name: "machine.yaml source is an explicit null",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "machine.yaml",
					strings.Replace(corpusGoodMachineYAML,
						"source: >-\n  Captured with scripts/collect-hardware-fixture.sh and reviewed file by file.\n",
						"source:\n", 1))
			},
			want: `missing required key "source"`,
		},
		{
			name: "machine.yaml allocatable.cpu is not a quantity",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "machine.yaml", strings.Replace(corpusGoodMachineYAML, `cpu: "2"`, `cpu: "two"`, 1))
			},
			want: "allocatable.cpu \"two\" is not a Kubernetes quantity",
		},
		{
			name: "writeRejectingZones names a zone that is not in the capture",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "machine.yaml",
					strings.Replace(corpusGoodMachineYAML, `"intel-rapl:0:0"`, `"intel-rapl:9:9"`, 1))
			},
			want: `writeRejectingZones names "intel-rapl:9:9"`,
		},
		// --- cpuinfo ---
		{
			name:   "cpuinfo has no processor block",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "cpuinfo", "this is not a cpuinfo\n") },
			want:   `cpuinfo: has no "processor" block`,
		},
		{
			name: "cpuinfo has no model name",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "cpuinfo", strings.ReplaceAll(corpusGoodCPUInfo, "model name", "model_name"))
			},
			want: `has no non empty "model name" line`,
		},
		{
			name: "cpuinfo physical id lines do not match the processor blocks",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "cpuinfo",
					strings.Replace(corpusGoodCPUInfo, "physical id\t: 1\n", "", 1))
			},
			want: `has 2 "processor" blocks but 1 "physical id" lines`,
		},
		{
			name: "cpuinfo physical id is not a number",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "cpuinfo",
					strings.Replace(corpusGoodCPUInfo, "physical id\t: 1", "physical id\t: one", 1))
			},
			want: `physical id "one" is not a non negative number`,
		},
		// --- powercap ---
		{
			name:   "zone without a name file",
			mutate: func(t *testing.T, dir string) { corpusRemove(t, dir, "powercap/intel-rapl:0/name") },
			want:   `powercap/intel-rapl:0: has no non empty "name" file`,
		},
		{
			name: "zone without a constraint_0 file",
			mutate: func(t *testing.T, dir string) {
				corpusRemove(t, dir, "powercap/intel-rapl:0/constraint_0_name")
				corpusRemove(t, dir, "powercap/intel-rapl:0/constraint_0_power_limit_uw")
				corpusRemove(t, dir, "powercap/intel-rapl:0/constraint_0_max_power_uw")
			},
			want: "powercap/intel-rapl:0: has no constraint_0_* file",
		},
		{
			name: "zone directory is not a zone name",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "powercap/socket0/name", "package-0\n")
				corpusWrite(t, dir, "powercap/socket0/constraint_0_max_power_uw", "165000000\n")
			},
			want: "powercap/socket0: is not a powercap zone name",
		},
		{
			name:   "unexpected file inside a zone",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "powercap/intel-rapl:0/uevent", "x\n") },
			want:   "powercap/intel-rapl:0/uevent: unexpected file in a powercap zone",
		},
		// --- GPU output ---
		{
			name:   "nvidia csv has too few columns",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "nvidia-smi.txt", "0, 70.00, Tesla T4\n") },
			want:   "has 3 comma separated fields, want 6",
		},
		{
			name: "nvidia csv kept its header",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "nvidia-smi.txt",
					"index, power.min_limit [W], power.max_limit [W], power.limit [W], power.draw [W], name\n"+corpusGoodNvidiaCSV)
			},
			want: "field 1 (index) is \"index\", not a number",
		},
		{
			name: "nvidia csv kept its units",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "nvidia-smi.txt", "0, 60.00 W, 70.00 W, 70.00 W, 9.97 W, Tesla T4\n")
			},
			want: "field 2 (power.min_limit) is \"60.00 W\", not a number",
		},
		{
			name:   "rocm output is not JSON",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "rocm-smi.txt", "GPU[0] : not json\n") },
			want:   "rocm-smi.txt: does not parse as a JSON object",
		},
		{
			name:   "rocm output has no card key",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "rocm-smi.txt", `{"system": {}}`) },
			want:   `has no "card<N>" key`,
		},
		// --- node labels ---
		{
			name:   "node labels are not an object",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "node-labels.json", `["a", "b"]`) },
			want:   "node-labels.json: is not a flat JSON object of strings",
		},
		{
			name:   "node label value is not a string",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "node-labels.json", `{"a": 1}`) },
			want:   `the value of "a" is 1`,
		},
		{
			name: "node labels still carry the hostname",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "node-labels.json", `{"kubernetes.io/hostname": "gpu-node-07"}`)
			},
			want: `kubernetes.io/hostname is "gpu-node-07"`,
		},
		// --- golden ---
		{
			name:   "expected.json does not parse",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "expected.json", "{not json}") },
			want:   "expected.json: does not parse as a NodeHardware status",
		},
		{
			name:   "expected.json was hand edited with an unknown field",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "expected.json", `{"cpu": {"tdpWatts": 165}}`) },
			want:   "does not parse as a NodeHardware status",
		},
		{
			name: "expected.json carries updatedAt",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "expected.json", `{"updatedAt": "2026-01-31T09:00:00Z"}`)
			},
			want: "carries updatedAt",
		},
		// --- privacy, one case per pattern ---
		{
			name:   "MAC address",
			mutate: func(t *testing.T, dir string) { corpusAppend(t, dir, "README.md", "link: 3c:ec:ef:1a:2b:3c\n") },
			want:   "contains a MAC address",
		},
		{
			name:   "IPv4 address",
			mutate: func(t *testing.T, dir string) { corpusAppend(t, dir, "README.md", "mgmt 10.24.13.7\n") },
			want:   "contains an IPv4 address",
		},
		{
			name: "IPv6 address",
			mutate: func(t *testing.T, dir string) {
				corpusAppend(t, dir, "README.md", "addr 2001:0db8:85a3:0000:0000:8a2e:0370:7334\n")
			},
			want: "contains an IPv6 address",
		},
		{
			name:   "compressed IPv6 address",
			mutate: func(t *testing.T, dir string) { corpusAppend(t, dir, "README.md", "addr fe80::1c2d:3e4f\n") },
			want:   "contains an IPv6 address",
		},
		{
			name: "UUID",
			mutate: func(t *testing.T, dir string) {
				corpusAppend(t, dir, "README.md", "product uuid 4c4c4544-0037-5610-8056-b8c04f435331\n")
			},
			want: "contains a UUID",
		},
		{
			name: "GPU UUID",
			mutate: func(t *testing.T, dir string) {
				corpusAppend(t, dir, "README.md", "GPU-1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f60\n")
			},
			want: "contains a GPU or MIG UUID",
		},
		{
			name:   "serial number",
			mutate: func(t *testing.T, dir string) { corpusAppend(t, dir, "README.md", "Serial Number: J7K2M9Q4\n") },
			want:   "contains a serial number or an asset tag",
		},
		{
			name:   "asset tag",
			mutate: func(t *testing.T, dir string) { corpusAppend(t, dir, "README.md", "Asset Tag: INV-884213\n") },
			want:   "contains a serial number or an asset tag",
		},
		{
			name: "hostname in a node label",
			mutate: func(t *testing.T, dir string) {
				corpusWrite(t, dir, "node-labels.json", `{"kubernetes.io/hostname": "node07.hpc.acme.corp"}`)
			},
			want: "contains a hostname or FQDN",
		},
		{
			name: "FQDN anywhere in the capture",
			mutate: func(t *testing.T, dir string) {
				corpusAppend(t, dir, "README.md", "captured on login1.cluster.example.eu\n")
			},
			want: "contains a hostname or FQDN",
		},
		{
			name:   "home directory path",
			mutate: func(t *testing.T, dir string) { corpusAppend(t, dir, "README.md", "dumped to /home/mbunino/capture\n") },
			want:   "contains a home directory path",
		},
		{
			name:   "username",
			mutate: func(t *testing.T, dir string) { corpusAppend(t, dir, "README.md", "user: mbunino\n") },
			want:   "contains a username",
		},
		{
			name:   "cloud instance id",
			mutate: func(t *testing.T, dir string) { corpusAppend(t, dir, "README.md", "ec2 i-0abcdef1234567890\n") },
			want:   "contains a cloud instance id",
		},
		{
			name: "cloud provider id",
			mutate: func(t *testing.T, dir string) {
				corpusAppend(t, dir, "README.md", "providerID: aws:///eu-west-1a/i-0abc\n")
			},
			want: "contains a cloud",
		},
		{
			name:   "binary file",
			mutate: func(t *testing.T, dir string) { corpusWrite(t, dir, "README.md", "head\x00\x01\x02tail") },
			want:   "is not a text file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, machine, dir := newCorpusTestMachine(t)
			tc.mutate(t, dir)
			msgs := validateCorpusMachine(root, machine)
			for _, msg := range msgs {
				if strings.Contains(msg, tc.want) {
					return
				}
			}
			t.Fatalf("no message contained %q. Got:\n%s", tc.want, strings.Join(msgs, "\n"))
		})
	}
}

func corpusRemove(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}
