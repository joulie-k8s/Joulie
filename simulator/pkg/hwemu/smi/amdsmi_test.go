package smi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// static --limit --json changed shape between 7.1 and 7.2: flat power keys in
// 7.1, one block per PPT in 7.2. Both use the gpu_data wrapper and indent 4.
func TestS08StaticLimitJSONShapes(t *testing.T) {
	t.Parallel()
	watts := func(w float64) any { return map[string]any{"value": w, "unit": "W"} }
	for _, v := range []string{amdsmiVariant71, amdsmiVariant72} {
		t.Run(v, func(t *testing.T) {
			n := newMI300XNode(t, "", v)
			n.write(hwmonRel(0, "power1_cap"), "600000000", 0o644)
			code, stdout, stderr := n.main("", "amd-smi", "static", "-g", "0", "--limit", "--json")
			if code != 0 || stderr != "" {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			if !strings.HasPrefix(stdout, "{\n    \"gpu_data\": [\n        {\n            \"gpu\": 0,\n            \"limit\": {\n") {
				t.Fatalf("output does not start as json.dumps(indent=4) of gpu_data:\n%s", stdout)
			}
			var doc struct {
				GPUData []struct {
					GPU   int            `json:"gpu"`
					Limit map[string]any `json:"limit"`
				} `json:"gpu_data"`
			}
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.GPUData) != 1 || doc.GPUData[0].GPU != 0 {
				t.Fatalf("gpu_data %+v, want GPU 0 only", doc.GPUData)
			}
			limit := doc.GPUData[0].Limit
			var want map[string]any
			if v == amdsmiVariant71 {
				want = map[string]any{"max_power": watts(750), "min_power": watts(0), "socket_power": watts(600)}
				if _, ok := limit["ppt0"]; ok {
					t.Errorf("7.1 has a ppt0 block: %v", limit)
				}
			} else {
				na := map[string]any{"max_power_limit": "N/A", "min_power_limit": "N/A", "socket_power_limit": "N/A"}
				want = map[string]any{
					"ppt0": map[string]any{"max_power_limit": watts(750), "min_power_limit": watts(0), "socket_power_limit": watts(600)},
					"ppt1": na,
				}
				if _, ok := limit["max_power"]; ok {
					t.Errorf("7.2 has the 7.1 max_power key: %v", limit)
				}
			}
			for k, w := range want {
				if !reflect.DeepEqual(limit[k], w) {
					t.Errorf("limit %q = %v, want %v", k, limit[k], w)
				}
			}
		})
	}
}

// set -o changed syntax between 7.1 (-o WATTS) and 7.2 (-o PPT WATTS); each
// rejects the other's form. Every outcome of a well-formed set exits 0, an
// out-of-range value writes nothing, and 7.2 names the PPT.
func TestS09SetPowerCapSyntaxAndOutcomes(t *testing.T) {
	t.Parallel()
	type tc struct {
		variant string
		args    []string
		code    int
		stream  string // "stdout" or "stderr"
		want    string
		cap     string // power1_cap of GPU 0 afterwards
	}
	cases := []tc{
		{amdsmiVariant71, []string{"-o", "300"}, 0, "stdout", "GPU: 0\n    POWERCAP: Successfully set power cap to 300W\n\n", "300000000"},
		{amdsmiVariant72, []string{"-o", "300"}, 2, "stderr", "amd-smi set: error: argument -o/--power-cap: expected 2 arguments\n", "750000000"},
		{amdsmiVariant72, []string{"-o", "ppt0", "300"}, 0, "stdout", "GPU: 0\n    POWERCAP: Successfully set PPT0 power cap to 300W\n\n", "300000000"},
		{amdsmiVariant71, []string{"-o", "ppt0", "300"}, 1, "stderr", "amdsmi_cli_exceptions.AmdSmiInvalidParameterValueException: Value 'ppt0' is not of valid type or format. Run 'amd-smi set -h' for more info. Error code: -5\n", "750000000"},
		{amdsmiVariant72, []string{"-o", "ppt0", "800"}, 0, "stdout", "GPU: 0\n    POWERCAP: Power cap must be between 1W and 750W\n\n", "750000000"},
		{amdsmiVariant71, []string{"-o", "800"}, 0, "stdout", "GPU: 0\n    POWERCAP: Power cap must be between 1W and 750W\n\n", "750000000"},
		{amdsmiVariant72, []string{"-o", "ppt0", "750"}, 0, "stdout", "GPU: 0\n    POWERCAP: PPT0 power cap is already set to 750W\n\n", "750000000"},
		{amdsmiVariant72, []string{"-o", "ppt1", "300"}, 0, "stdout", "GPU: 0\n    POWERCAP: [AMDSMI_STATUS_NOT_SUPPORTED] Unable to set PPT1 power cap to 300W\n\n", "750000000"},
		{amdsmiVariant72, []string{"-o", "ppt2", "300"}, 1, "stderr", "amdsmi_cli_exceptions.AmdSmiInvalidParameterException: Parameter 'ppt2' is invalid. Run 'amd-smi set -h' for more info. Error code: -2\n", "750000000"},
		{amdsmiVariant72, []string{"-o", "PPT0", "300", "--json"}, 0, "stdout", "[\n    {\n        \"gpu\": 0,\n        \"powercap\": \"Successfully set PPT0 power cap to 300W\"\n    }\n]\n", "300000000"},
	}
	for _, c := range cases {
		t.Run(c.variant+" "+strings.Join(c.args, " "), func(t *testing.T) {
			n := newMI300XNode(t, "", c.variant)
			code, stdout, stderr := n.main("", append([]string{"amd-smi", "set", "-g", "0"}, c.args...)...)
			got := stdout
			if c.stream == "stderr" {
				got = stderr
				if stdout != "" {
					t.Errorf("stdout %q, want empty", stdout)
				}
			}
			if code != c.code || !strings.HasSuffix(got, c.want) {
				t.Fatalf("exit %d, %s %q; want %d and %q", code, c.stream, got, c.code, c.want)
			}
			if p := n.read(hwmonRel(0, "power1_cap")); p != c.cap {
				t.Fatalf("power1_cap = %s, want %s", p, c.cap)
			}
			if p := n.read(hwmonRel(1, "power1_cap")); p != "750000000" {
				t.Fatalf("-g 0 changed GPU 1: power1_cap = %s", p)
			}
		})
	}
}

// reset -o writes power1_cap_default back in both versions.
func TestS12ResetPowerCapWritesDefault(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]string{
		amdsmiVariant71: "GPU: 0\n    POWERCAP: Successfully set power cap to 700W\n\n",
		amdsmiVariant72: "GPU: 0\n    POWERCAP:\n        PPT0: Successfully reset power cap to 700W\n        PPT1: [AMDSMI_STATUS_NOT_SUPPORTED] Unable to reset to default power cap\n\n",
	} {
		t.Run(v, func(t *testing.T) {
			n := newMI300XNode(t, "", v)
			n.write(hwmonRel(0, "power1_cap_default"), "700000000", 0o444)
			n.write(hwmonRel(0, "power1_cap"), "500000000", 0o644)
			code, stdout, stderr := n.main("", "amd-smi", "reset", "-g", "0", "-o")
			if code != 0 || stderr != "" || stdout != want {
				t.Fatalf("exit %d, stderr %q, stdout %q; want 0 and %q", code, stderr, stdout, want)
			}
			if got := n.read(hwmonRel(0, "power1_cap")); got != "700000000" {
				t.Fatalf("power1_cap = %s, want the default 700000000", got)
			}
		})
	}
}

// SG05: set --perf-level HIGH writes high and reports success; a family
// that refuses HIGH gets the options list and the message, and the level
// stays; without root the command raises PermissionError (cmds
// 7.2:4731-4746).
func TestSG05SetPerfLevel(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, "", amdsmiVariant72)
	n.dpm(dpmNavi)
	code, stdout, stderr := n.main("", "amd-smi", "set", "-g", "0", "--perf-level", "high")
	if code != 0 || stderr != "" || stdout != "GPU: 0\n    PERFLEVEL: Successfully set performance level HIGH\n\n" {
		t.Fatalf("HIGH: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := n.read(dpmLevelRel(0)); got != "high" {
		t.Errorf("level after HIGH: %q", got)
	}
	if code, stdout, _ := n.main("", "amd-smi", "set", "-g", "1", "-l", "STABLE_PEAK", "--json"); code != 0 || !strings.Contains(stdout, `"perflevel": "Successfully set performance level STABLE_PEAK"`) || n.read(dpmLevelRel(1)) != "profile_peak" {
		t.Errorf("STABLE_PEAK --json: exit %d, stdout %q, level %q", code, stdout, n.read(dpmLevelRel(1)))
	}
	if code, _, stderr := n.main("", "amd-smi", "set", "-g", "0", "--perf-level", "turbo"); code != 2 || !strings.Contains(stderr, "argument -l/--perf-level: invalid choice: 'TURBO' (choose from 'AUTO', ") {
		t.Errorf("TURBO: exit %d, stderr %q", code, stderr)
	}

	m := newMI300XNode(t, "", amdsmiVariant72)
	m.dpm(dpmAqua)
	code, stdout, _ = m.main("", "amd-smi", "set", "-g", "0", "--perf-level", "HIGH")
	want := "\nPerformance Level Options:\n\tAUTO,LOW,HIGH,MANUAL,STABLE_STD,STABLE_PEAK,STABLE_MIN_MCLK,STABLE_MIN_SCLK,DETERMINISM\n\n" +
		"GPU: 0\n    PERFLEVEL: [AMDSMI_STATUS_INVAL] Unable to set performance level to HIGH\n\n"
	if code != 0 || stdout != want {
		t.Errorf("HIGH refused: exit %d, stdout %q, want %q", code, stdout, want)
	}
	if got := m.read(dpmLevelRel(0)); got != "auto" {
		t.Errorf("a refused HIGH was written: %q", got)
	}

	m.env.Unprivileged = true
	if code, stdout, stderr := m.main("", "amd-smi", "set", "-g", "0", "--perf-level", "manual"); code != 1 || stdout != "" || stderr != "PermissionError: Command requires elevation\n" {
		t.Errorf("unprivileged: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := m.read(dpmLevelRel(0)); got != "auto" {
		t.Errorf("an unprivileged set was written: %q", got)
	}
}

// SG06: set --profile is parsed and does nothing but print Not Yet
// Implemented (cmds 7.2:4747-4751).
func TestSG06SetProfileIsNotYetImplemented(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, "", amdsmiVariant72)
	n.dpm(dpmNavi)
	before := n.read(dpmDevRel(0, layout.AMDGPUPowerProfileFile))
	code, stdout, stderr := n.main("", "amd-smi", "set", "-g", "0", "--profile", "COMPUTE")
	if code != 0 || stderr != "" || stdout != "GPU: 0\n    PROFILE: Not Yet Implemented\n\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if n.read(dpmDevRel(0, layout.AMDGPUPowerProfileFile)) != before || n.read(dpmLevelRel(0)) != "auto" {
		t.Errorf("set --profile changed the profile or the level")
	}
	if code, _, stderr := n.main("", "amd-smi", "set", "-g", "0", "--profile", "COMPUTE", "-l", "HIGH"); code != 2 || !strings.Contains(stderr, "argument -l/--perf-level: not allowed with argument -P/--profile") {
		t.Errorf("two options of the exclusive group: exit %d, stderr %q", code, stderr)
	}
}

// SG07: set -L sclk max sets the level to manual, then writes the edit and
// its commit to pp_od_clk_voltage in one write (rocm_smi.cc:1508-1560); the
// emulator applies both (TestGD08OverdriveOnlyUnderManual runs it through a
// Node). An uclk minimum is not supported, a bound outside the table is
// refused, and a family without overdrive reports not supported after the
// level moved to manual.
func TestSG07ClkLimitGoesThroughManualAndOverdrive(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, "", amdsmiVariant72)
	n.dpm(dpmAqua)
	code, stdout, stderr := n.main("", "amd-smi", "set", "-g", "0", "-L", "sclk", "max", "1200")
	if code != 0 || stderr != "" || stdout != "GPU: 0\n    CLK_LIMIT: Successfully changed max of sclk to 1200MHz\n\n" {
		t.Fatalf("sclk max 1200: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if lvl, od := n.read(dpmLevelRel(0)), n.read(dpmDevRel(0, layout.AMDGPUODFile)); lvl != "manual" || od != "s 1 1200\nc" {
		t.Errorf("after sclk max 1200: level %q, pp_od_clk_voltage %q; want manual and the edit with its commit", lvl, od)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-L", "mclk", "min", "600"}, "Setting mclk min is not supported"},
		{[]string{"-L", "sclk", "max", "1500"}, "Clock limit is already set to 1500MHz"},
		{[]string{"-L", "sclk", "max", "200"}, "Cannot set sclk max value less than min (300MHz)"},
		{[]string{"-L", "sclk", "min", "1600"}, "Cannot set sclk min value greater than max (1500MHz)"},
		{[]string{"-L", "sclk", "min", "200"}, "[AMDSMI_STATUS_INVAL] Unable to set min of sclk to 200MHz"},
	} {
		code, stdout, _ := n.main("", append([]string{"amd-smi", "set", "-g", "1"}, c.args...)...)
		if code != 0 || !strings.Contains(stdout, c.want) {
			t.Errorf("%v: exit %d, stdout %q; want %q", c.args, code, stdout, c.want)
		}
	}
	if code, _, stderr := n.main("", "amd-smi", "set", "-g", "1", "-L", "fclk", "max", "1"); code != 1 || !strings.Contains(stderr, "AmdSmiInvalidParameterException") {
		t.Errorf("-L fclk: exit %d, stderr %q; want the invalid parameter exception", code, stderr)
	}

	w := newMI300XNode(t, "", amdsmiVariant72)
	w.dpm(dpmNavi)
	if _, stdout, _ := w.main("", "amd-smi", "set", "-g", "0", "-L", "sclk", "max", "1200"); !strings.Contains(stdout, "[AMDSMI_STATUS_NOT_SUPPORTED] Unable to set max of sclk to 1200MHz") || w.read(dpmLevelRel(0)) != "manual" {
		t.Errorf("no overdrive: stdout %q, level %q; want not supported, the level already at manual", stdout, w.read(dpmLevelRel(0)))
	}
	// NOT_SUPPORTED for an mclk minimum prints its own line, not the status
	// (cmds 7.2:5163-5165).
	if _, stdout, _ := w.main("", "amd-smi", "set", "-g", "1", "-L", "mclk", "min", "600"); stdout != "GPU: 1\n    CLK_LIMIT: Setting mclk min is not supported\n\n" {
		t.Errorf("no overdrive, mclk min: stdout %q; want Setting mclk min is not supported", stdout)
	}
}
