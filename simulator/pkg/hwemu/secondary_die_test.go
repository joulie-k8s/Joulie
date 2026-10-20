package hwemu

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// H45: a device in secondaryDies renders and behaves as the secondary die
// of an Aldebaran package (Linux v6.17 aldebaran_ppt.c:1239-1306,
// amdgpu_pm.c:3478-3484): power1_cap and power1_cap_default read 0,
// power1_cap_min and power1_cap_max read the package's range, and there is
// no power1_input or power1_average. CheckWrite of any value fails with
// EINVAL, one that does not parse too, and changes nothing; a write that
// lands in the file is reverted at the next step, an EventRejected with
// EINVAL. A primary of the same node takes its cap, and a secondary die,
// with no limit of its own, draws its uncapped power at full load.
func TestH45SecondaryDieHasNoCapOfItsOwn(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, "amd-instinct-mi300x-8gpu")
	p.GPUs.SecondaryDies = []int{1, 3, 5, 7}
	tr := renderTestTree(t, p)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{GPUUtil: []float64{1}})

	for i := 0; i < p.GPUs.Count.V; i++ {
		secondary := i%2 == 1
		want := map[string]string{
			"power1_cap":         "750000000\n",
			"power1_cap_default": "750000000\n",
			"power1_cap_min":     "0\n",
			"power1_cap_max":     "750000000\n",
		}
		if secondary {
			want["power1_cap"], want["power1_cap_default"] = "0\n", "0\n"
		}
		for f, w := range want {
			if got := nodeRead(t, tr, nodeMI300XCap(i, f)); got != w {
				t.Errorf("gpu %d %s = %q, want %q", i, f, got, w)
			}
		}
		_, err := os.Stat(filepath.Join(tr.Root, filepath.FromSlash(nodeMI300XCap(i, "power1_input"))))
		if secondary && err == nil {
			t.Errorf("secondary die %d has power1_input; the driver hides a sensor that fails with EOPNOTSUPP", i)
		}
		if !secondary && err != nil {
			t.Errorf("primary %d has no power1_input: %v", i, err)
		}
		if _, err := os.Stat(filepath.Join(tr.Root, filepath.FromSlash(nodeMI300XCap(i, "power1_average")))); err == nil {
			t.Errorf("gpu %d has power1_average; the profile's sensor is power1_input", i)
		}
	}

	sec := nodeMI300XCap(1, "power1_cap")
	for _, v := range []string{"300000000", "750000000", "0", "800000000", "300 W"} {
		if err := n.CheckWrite(sec, v); !errors.Is(err, syscall.EINVAL) {
			t.Errorf("CheckWrite(%s) on the secondary die: %v, want EINVAL", v, err)
		}
	}
	if err := n.CheckWrite(nodeMI300XCap(0, "power1_cap"), "300000000"); err != nil {
		t.Errorf("CheckWrite of 300 W on the primary: %v", err)
	}
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, sec); got != "0\n" {
		t.Errorf("secondary die power1_cap = %q after refused writes, want 0", got)
	}

	nodeWrite(t, tr, sec, "300000000")
	nodeStep(t, n, 5*time.Second)
	if got := nodeRead(t, tr, sec); got != "0\n" {
		t.Errorf("a write that landed on the secondary die reads back %q, want 0", got)
	}
	if e := nodeOneEvent(t, n, sec); e.Kind != EventRejected || e.Errno != ErrnoEINVAL || e.Written != "300000000" || e.Effective != "0" {
		t.Errorf("event = %+v, want rejected with EINVAL keeping 0", e)
	}
	g := n.State().GPUs
	if g[0].LimitW != 300 || g[0].PowerW > 303 {
		t.Errorf("primary 0 limit %v W, power %.1f W; want the 300 W cap in force", g[0].LimitW, g[0].PowerW)
	}
	if g[1].LimitW != 0 || g[1].PowerW < 600 {
		t.Errorf("secondary die 1 limit %v W, power %.1f W; want no limit of its own and its uncapped power", g[1].LimitW, g[1].PowerW)
	}
}

// A profile with secondary dies has no fast PPT: only Van Gogh has power2,
// and the GPUs with secondary dies are Aldebaran's.
func TestH45SecondaryDiesNeedNoFastPPT(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, "amd-instinct-mi300x-8gpu")
	p.GPUs.SecondaryDies = []int{1}
	p.GPUs.PPT1 = Fact[string]{V: "present", Src: "assumed:test"}
	if err := p.Validate(); err == nil {
		t.Fatal("a profile with secondary dies and ppt1 present validates")
	}
}
