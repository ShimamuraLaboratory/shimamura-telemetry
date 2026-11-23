package collector

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shirou/gopsutil/v4/sensors"
)

func temps(kv ...any) []sensors.TemperatureStat {
	var out []sensors.TemperatureStat
	for i := 0; i < len(kv); i += 2 {
		out = append(out, sensors.TemperatureStat{SensorKey: kv[i].(string), Temperature: kv[i+1].(float64)})
	}
	return out
}

func TestCPUTemperature(t *testing.T) {
	tests := []struct {
		name  string
		stats []sensors.TemperatureStat
		want  *float64
	}{
		{
			name: "intel coretemp: package",
			stats: temps(
				"coretemp_package_id_0", 61.0,
				"coretemp_core_0", 65.0, // コアの方が高くてもパッケージを使う
				"coretemp_core_1", 58.0,
			),
			want: ptr(61),
		},
		{
			name: "intel coretemp: multiple sockets → the hottest package",
			stats: temps(
				"coretemp_package_id_0", 55.0,
				"coretemp_core_0", 54.0,
				"coretemp_package_id_1", 72.5,
				"coretemp_core_0", 71.0,
			),
			want: ptr(72.5),
		},
		{
			name:  "intel coretemp: no package → the hottest core",
			stats: temps("coretemp_core_0", 48.0, "coretemp_core_8", 53.0),
			want:  ptr(53),
		},
		{
			name:  "intel without hwmon: thermal_zone x86_pkg_temp",
			stats: temps("acpitz", 30.0, "x86_pkg_temp", 47.0),
			want:  ptr(47),
		},
		{
			name: "amd k10temp: Tdie is preferred over Tctl",
			stats: temps(
				"k10temp_tctl", 77.0, // オフセットが乗っている
				"k10temp_tdie", 50.0,
				"k10temp_tccd1", 45.0,
			),
			want: ptr(50),
		},
		{
			name:  "amd k10temp: only Tctl",
			stats: temps("k10temp_tctl", 63.25, "k10temp_tccd1", 60.0),
			want:  ptr(63.25),
		},
		{
			name: "amd k10temp: multiple sockets",
			stats: temps(
				"k10temp_tctl", 58.0,
				"k10temp_tctl", 81.0,
			),
			want: ptr(81),
		},
		{
			name:  "amd zenpower",
			stats: temps("zenpower_tdie", 44.0, "zenpower_tctl", 54.0),
			want:  ptr(44),
		},
		{
			name:  "arm cpu_thermal",
			stats: temps("cpu_thermal", 52.0),
			want:  ptr(52),
		},
		{
			name: "no cpu sensors (nvme, gpu, acpi, chipset, wifi)",
			stats: temps(
				"nvme_composite", 40.0,
				"amdgpu_edge", 60.0,
				"acpitz", 27.8,
				"pch_cannonlake", 50.0,
				"iwlwifi_1", 45.0,
			),
			want: nil,
		},
		{
			name:  "no sensors (vm)",
			stats: nil,
			want:  nil,
		},
		{
			name: "out-of-range values are ignored",
			stats: temps(
				"coretemp_package_id_0", 255.0, // センサーの異常値
				"coretemp_package_id_1", 66.0,
			),
			want: ptr(66),
		},
		{
			name:  "only out-of-range values → null",
			stats: temps("k10temp_tctl", -273.0, "k10temp_tdie", 500.0),
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cpuTemperature(tt.stats)
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("cpuTemperature() = %v, want %v", deref(got), deref(tt.want))
			}
		})
	}
}

func TestSystemCollectCPUTemperature(t *testing.T) {
	src := baseSource()
	src.temps = temps("coretemp_package_id_0", 61.0, "nvme_composite", 40.0)
	snap, err := newTestSystem(src).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Metrics.CPU.TemperatureC; got == nil || *got != 61 {
		t.Errorf("temperatureC = %v", deref(got))
	}

	// 一部のセンサーが読めなくても（警告付き）、読めた分から選ぶ
	src.tempsErr = errors.New("warnings: 1 sensor failed")
	snap, err = newTestSystem(src).Collect(t.Context())
	if err != nil || snap.Metrics.CPU.TemperatureC == nil {
		t.Fatalf("temperatureC = %v, %v", deref(snap.Metrics.CPU.TemperatureC), err)
	}

	// 読めなくても、他の値は送り、温度だけ null にする
	src.temps, src.tempsErr = nil, errors.New("no hwmon")
	snap, err = newTestSystem(src).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Metrics.CPU.TemperatureC != nil {
		t.Errorf("temperatureC = %v, want nil", deref(snap.Metrics.CPU.TemperatureC))
	}
	b, _ := json.Marshal(snap.Metrics.CPU)
	if !strings.Contains(string(b), `"temperatureC":null`) || !strings.Contains(string(b), `"usagePercent"`) {
		t.Errorf("cpu = %s", b)
	}
}
