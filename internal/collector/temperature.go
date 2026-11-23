package collector

import (
	"strings"

	"github.com/shirou/gopsutil/v4/sensors"
)

// CPU の温度（異常な発熱の通知用、Issue #19）。
//
// gopsutil の sensors.Temperatures は /sys/class/hwmon の各センサーを「<チップ名>_<ラベル>」の SensorKey で返す
// （ラベルは小文字にして空白を _ にしたもの。例: coretemp の "Package id 0" → "coretemp_package_id_0"）。
// hwmon が無い環境では /sys/class/thermal の thermal_zone の種類（例: "x86_pkg_temp"）を返す。
// この中から CPU のセンサーだけを選び、複数ソケットの場合は最も高い温度を送る。
// NVMe・GPU・ACPI（acpitz）などの CPU 以外のセンサーは使わない。

// cpuTemperature は CPU のパッケージ温度（℃）を選ぶ。CPU のセンサーが無い・範囲外の値しか無い場合は nil。
//
//   - Intel（coretemp）: "Package id N" の最大。無ければ "Core N" の最大
//   - AMD（k10temp・zenpower）: "Tdie" の最大。無ければ "Tctl" の最大（Tctl はオフセットが乗るモデルがあるため Tdie を優先する）
//   - hwmon が無い Intel: thermal_zone の "x86_pkg_temp"
//   - その他: 名前が "cpu" で始まるセンサー（ARM の "cpu_thermal" など）
//
// 範囲外の値（-50〜200℃ の外。センサーの異常値）は使わない。
func cpuTemperature(stats []sensors.TemperatureStat) *float64 {
	var intelPackage, intelCore, amdTdie, amdTctl, other []float64
	for _, s := range stats {
		key := strings.ToLower(s.SensorKey)
		t := s.Temperature
		if t < minTemperatureC || t > maxTemperatureC {
			continue
		}
		switch {
		case strings.HasPrefix(key, "coretemp_package_id_"), key == "x86_pkg_temp":
			intelPackage = append(intelPackage, t)
		case strings.HasPrefix(key, "coretemp_core_"):
			intelCore = append(intelCore, t)
		case isAMDChip(key) && strings.HasSuffix(key, "_tdie"):
			amdTdie = append(amdTdie, t)
		case isAMDChip(key) && strings.HasSuffix(key, "_tctl"):
			amdTctl = append(amdTctl, t)
		case strings.HasPrefix(key, "cpu"):
			other = append(other, t)
		}
	}

	var picked []float64
	picked = append(picked, preferred(intelPackage, intelCore)...)
	picked = append(picked, preferred(amdTdie, amdTctl)...)
	picked = append(picked, other...)
	if len(picked) == 0 {
		return nil
	}
	v := round2(maxOf(picked))
	return &v
}

// isAMDChip は AMD の CPU の温度センサーのチップか（k10temp・zenpower）を返す。
func isAMDChip(key string) bool {
	return strings.HasPrefix(key, "k10temp_") || strings.HasPrefix(key, "zenpower_")
}

// preferred は first があれば first を、無ければ fallback を返す。
func preferred(first, fallback []float64) []float64 {
	if len(first) > 0 {
		return first
	}
	return fallback
}

func maxOf(vs []float64) float64 {
	m := vs[0]
	for _, v := range vs[1:] {
		m = max(m, v)
	}
	return m
}
