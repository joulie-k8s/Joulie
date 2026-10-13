package main

import (
	"bytes"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// profilesEnvLocalXeon is a local profile on top of the builtin Intel host.
const profilesEnvLocalXeon = `schemaVersion: 1
name: lab-xeon
extends: intel-xeon-2s-rapl
description: the builtin Intel host with a local idle figure
physics:
  cpu:
    idlePkgW: {v: 55, src: "assumed:local test override"}
`

const profilesEnvLogPrefix = "local profiles loaded "

// profilesEnvLog is the standard logger's output for one test. The lock lets
// a goroutine an earlier test left behind log while the test reads.
type profilesEnvLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *profilesEnvLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// lines returns the logged lines about local profiles.
func (l *profilesEnvLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if i := strings.Index(line, profilesEnvLogPrefix); i >= 0 {
			out = append(out, line[i:])
		}
	}
	return out
}

// profilesEnvCapture sends the standard logger to a buffer and resets the
// log-once guard, so each test sees its own first successful call.
func profilesEnvCapture(t *testing.T) *profilesEnvLog {
	t.Helper()
	l := &profilesEnvLog{}
	prev := log.Writer()
	log.SetOutput(l)
	logLocalProfilesOnce = sync.Once{}
	t.Cleanup(func() {
		log.SetOutput(prev)
		logLocalProfilesOnce = sync.Once{}
	})
	return l
}

// Unset, empty or only spaces, SIM_PROFILES_DIR means the builtin profiles,
// and nothing is logged about local ones.
func TestLoadProfileSetFromEnvUnsetMeansBuiltins(t *testing.T) {
	logs := profilesEnvCapture(t)
	builtins, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envProfilesDir, "")
	if err := os.Unsetenv(envProfilesDir); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"unset", "", " \t\n"} {
		if value != "unset" {
			t.Setenv(envProfilesDir, value)
		}
		profiles, err := loadProfileSetFromEnv()
		if err != nil {
			t.Fatalf("%s=%q: %v", envProfilesDir, value, err)
		}
		if !reflect.DeepEqual(profiles, builtins) {
			t.Errorf("%s=%q: want exactly the builtin profiles", envProfilesDir, value)
		}
	}
	if got := logs.lines(); len(got) != 0 {
		t.Errorf("logged %q with no local profiles", got)
	}
}

// A directory, with the spaces around it trimmed, adds its profiles to the
// builtins. Every call parses afresh, because a cluster spec changes the copy
// each pool gets, and the local names are logged once, not once per pool.
func TestLoadProfileSetFromEnvLoadsLocalProfilesAndLogsOnce(t *testing.T) {
	logs := profilesEnvCapture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lab-xeon.yaml"), []byte(profilesEnvLocalXeon), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envProfilesDir, " "+dir+"\n")
	builtins, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}

	var first map[string]*hwemu.Profile
	for call := 1; call <= 3; call++ {
		profiles, err := loadProfileSetFromEnv()
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		x := profiles["lab-xeon"]
		if x == nil || x.Physics.CPU.IdlePkgW.V != 55 || len(profiles) != len(builtins)+1 {
			t.Fatalf("call %d: got %d profiles, lab-xeon %v: want the %d builtins plus lab-xeon at 55 W idle", call, len(profiles), x, len(builtins))
		}
		if first == nil {
			first = profiles
		} else if profiles["lab-xeon"] == first["lab-xeon"] || profiles["intel-xeon-2s-rapl"] == first["intel-xeon-2s-rapl"] {
			t.Errorf("call %d shares profiles with call 1; each pool must get its own copy", call)
		}
	}
	want := profilesEnvLogPrefix + "dir=" + dir + " profiles=lab-xeon"
	if got := logs.lines(); len(got) != 1 || got[0] != want {
		t.Errorf("logged %q, want once %q", got, want)
	}
}

// A directory that does not exist is an error naming the variable and its
// value, never a fallback to the builtins. A failed call logs nothing and
// leaves the log to the first call that succeeds.
func TestLoadProfileSetFromEnvMissingDirNamesTheVariable(t *testing.T) {
	logs := profilesEnvCapture(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-dir")
	t.Setenv(envProfilesDir, missing)
	profiles, err := loadProfileSetFromEnv()
	want := envProfilesDir + `="` + missing + `": local profiles: `
	if err == nil || !strings.HasPrefix(err.Error(), want) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want a not-exist error starting with %q", err, want)
	}
	if profiles != nil {
		t.Errorf("got %d profiles with the error, want none", len(profiles))
	}

	if err := os.WriteFile(filepath.Join(dir, "lab-xeon.yaml"), []byte(profilesEnvLocalXeon), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envProfilesDir, dir)
	if _, err := loadProfileSetFromEnv(); err != nil {
		t.Fatal(err)
	}
	if got := logs.lines(); len(got) != 1 || !strings.HasSuffix(got[0], " profiles=lab-xeon") {
		t.Errorf("logged %q, want one line for the call that succeeded", got)
	}
}
