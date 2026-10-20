package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// SIM_HWEMU_FUSE is off unless set to one of boolEnv's true words; an
// unknown word keeps the default.
func TestEmulateOptionsFUSE(t *testing.T) {
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 10, prometheus.NewRegistry())
	for _, c := range []struct {
		value string
		set   bool
		want  bool
	}{
		{"", false, false},
		{"true", true, true},
		{"On", true, true},
		{"0", true, false},
		{"maybe", true, false},
	} {
		env := map[string]string{envClusterSpec: "/s.yaml"}
		if c.set {
			env[envHwemuFUSE] = c.value
		}
		o, err := s.emulateOptionsFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		if err != nil {
			t.Fatal(err)
		}
		if o.FUSE != c.want {
			t.Errorf("%s=%q: FUSE %t, want %t", envHwemuFUSE, c.value, o.FUSE, c.want)
		}
	}
}
