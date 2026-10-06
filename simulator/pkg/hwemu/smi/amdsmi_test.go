package smi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
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
