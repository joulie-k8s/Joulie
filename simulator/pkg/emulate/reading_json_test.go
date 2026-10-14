package emulate

import (
	"encoding/json"
	"testing"
)

// The wire format of a component reading and a governor state, which
// /debug/emulate and every snapshot consumer decode: every field under its
// JSON name, the optional ones left out when zero.
func TestComponentReadingJSONIsStable(t *testing.T) {
	full := ComponentReading{
		ID: "gpu1", Type: "gpu", Name: "NVIDIA H100", Vendor: "NVIDIA", Model: "H100",
		UUID: "GPU-1", PCIBusID: "00000000:1B:00.0", SysfsPath: "devices/pci0000:1b", Parent: "cpu0",
		Index: 1, Measured: true, PowerW: 1.5, EnergyJ: 2.5, DeviceEnergyJ: 3.5,
		LimitW: 4.5, RequestedLimitW: 5.5, SpeedHz: 6.5, SpeedMaxHz: 7.5, SpeedLimitHz: 8.5, Utilization: 0.25,
		ClockHz: map[string]int64{"graphics": 1}, ClockMaxHz: map[string]int64{"graphics": 2}, ClockLimitHz: map[string]int64{"graphics": 3},
		GovernorNative: "auto", GovernorPreset: "efficient", PeerRxBytes: 9, PeerTxBytes: 10,
	}
	for _, tc := range []struct {
		name string
		c    ComponentReading
		want string
	}{
		{"full", full, `{"id":"gpu1","type":"gpu","name":"NVIDIA H100","vendor":"NVIDIA","model":"H100","uuid":"GPU-1",` +
			`"pciBusId":"00000000:1B:00.0","sysfsPath":"devices/pci0000:1b","parent":"cpu0","index":1,"measured":true,` +
			`"powerW":1.5,"energyJ":2.5,"deviceEnergyJ":3.5,"limitW":4.5,"requestedLimitW":5.5,"speedHz":6.5,"speedMaxHz":7.5,` +
			`"speedLimitHz":8.5,"utilization":0.25,"clockHz":{"graphics":1},"clockMaxHz":{"graphics":2},"clockLimitHz":{"graphics":3},` +
			`"governorNative":"auto","governorPreset":"efficient","peerRxBytes":9,"peerTxBytes":10}`},
		{"zero", ComponentReading{}, `{"id":"","type":"","name":"","index":0,"measured":false,"powerW":0,"energyJ":0,"utilization":0}`},
	} {
		b, err := json.Marshal(tc.c)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, b, tc.want)
		}
		var back ComponentReading
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if again, _ := json.Marshal(back); string(again) != tc.want {
			t.Errorf("%s does not survive a round trip: %s", tc.name, again)
		}
	}
	for g, want := range map[GovernorState]string{
		{Native: "powersave epp=balance_power", Preset: "efficient"}: `{"native":"powersave epp=balance_power","preset":"efficient"}`,
		{}: `{"native":""}`,
	} {
		if b, err := json.Marshal(g); err != nil || string(b) != want {
			t.Errorf("governor state %+v encodes as %s (%v), want %s", g, b, err, want)
		}
	}
}
