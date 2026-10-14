package calib

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// DutyCycleLoad is the CPU load of a sweep on real hardware: one goroutine
// per CPU, each locked to its own OS thread and, on Linux, pinned to its
// CPU, spinning for the share u of every period and sleeping for the rest.
// The spin touches no memory: its memory intensity is 0.
type DutyCycleLoad struct {
	period time.Duration
	share  atomic.Uint64 // math.Float64bits of u
	stop   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

// sweepPin pins the calling OS thread to cpu. sweep_linux.go sets it.
var sweepPin = func(cpu int) error {
	return errors.New("pinning a thread to a CPU is not supported on " + runtime.GOOS)
}

// sweepSink keeps the spin's result alive, so the compiler keeps the loop.
var sweepSink atomic.Uint64

// NewDutyCycleLoad starts an idle load on cpus with a duty period (100 ms
// when zero). warn, when set, is called once when a thread cannot be
// pinned; the load then runs where the scheduler puts it. GOMAXPROCS is
// raised to one more than the CPUs, so the sampler always has a thread.
func NewDutyCycleLoad(cpus []int, period time.Duration, warn func(error)) *DutyCycleLoad {
	if period <= 0 {
		period = 100 * time.Millisecond
	}
	if n := len(cpus) + 1; runtime.GOMAXPROCS(0) < n {
		runtime.GOMAXPROCS(n)
	}
	l := &DutyCycleLoad{period: period, stop: make(chan struct{})}
	var warnOnce sync.Once
	for _, c := range cpus {
		l.wg.Add(1)
		go l.run(c, func(err error) {
			if warn != nil {
				warnOnce.Do(func() { warn(err) })
			}
		})
	}
	return l
}

// SetUtilization sets the busy share of every CPU, 0 to 1.
func (l *DutyCycleLoad) SetUtilization(u float64) error {
	if !(u >= 0 && u <= 1) {
		return fmt.Errorf("utilization %v is outside 0..1", u)
	}
	l.share.Store(math.Float64bits(u))
	return nil
}

// Stop ends the load and waits for every goroutine.
func (l *DutyCycleLoad) Stop() {
	l.once.Do(func() { close(l.stop) })
	l.wg.Wait()
}

// run is the load of one CPU. The goroutine keeps its thread locked when it
// returns, so the runtime ends the thread rather than reuse one pinned to a
// single CPU.
func (l *DutyCycleLoad) run(cpu int, warn func(error)) {
	defer l.wg.Done()
	runtime.LockOSThread()
	if err := sweepPin(cpu); err != nil {
		warn(fmt.Errorf("CPU %d: %w", cpu, err))
	}
	x := uint64(cpu) + 1
	for {
		begin := time.Now()
		busy := time.Duration(math.Float64frombits(l.share.Load()) * float64(l.period))
		for end := begin.Add(busy); time.Now().Before(end); {
			for i := 0; i < 4096; i++ {
				x = x*6364136223846793005 + 1442695040888963407
			}
		}
		rest := l.period - time.Since(begin)
		if rest <= 0 {
			select {
			case <-l.stop:
				sweepSink.Store(x)
				return
			default:
				continue
			}
		}
		t := time.NewTimer(rest)
		select {
		case <-l.stop:
			t.Stop()
			sweepSink.Store(x)
			return
		case <-t.C:
		}
	}
}
