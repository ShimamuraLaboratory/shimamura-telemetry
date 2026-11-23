// Package collector は計算機サーバーのリソース情報を収集する処理を提供する。
//
// 型は shimamura-IdP の pkg/schema/src/serverMonitor/messages.ts（プロトコルバージョン 1）の
// ServerMetricsSchema に合わせる。JSON のキー名も同じにしているため、そのまま送信メッセージに埋め込める。
package collector

import (
	"context"
	"time"
)

// メッセージ形式で決まっている配列の上限。超えた分は送らない。
const (
	MaxCores = 512
	MaxDisks = 64
	MaxGPUs  = 16
	// MaxMountLength はマウントポイントの文字数（バイト数ではない）の上限。
	MaxMountLength = 255
)

// Snapshot はある時点で収集したリソース情報。
type Snapshot struct {
	// CollectedAt は収集した時刻。送信メッセージの measuredAt になる。
	CollectedAt time.Time
	// Metrics は収集した値。
	Metrics Metrics
}

// Metrics は ServerMetricsSchema に対応する。
type Metrics struct {
	CPU    CPUMetrics    `json:"cpu"`
	Memory MemoryMetrics `json:"memory"`
	// Disks はマウントごとの使用量。null にならないよう空でもスライスを入れること。
	Disks []DiskMetrics `json:"disks"`
	// GPUs は GPU ごとの情報。null にならないよう空でもスライスを入れること。
	GPUs []GPUMetrics `json:"gpus"`
	// Processes は任意。nil のときは送らない。
	Processes []ProcessMetrics `json:"processes,omitempty"`
}

// CPUMetrics は CpuMetricsSchema に対応する。
type CPUMetrics struct {
	// UsagePercent は全体の使用率（0〜100）。
	UsagePercent float64 `json:"usagePercent"`
	// Cores はコアごとの使用率（0〜100）。
	Cores []float64 `json:"cores"`
	// TemperatureC は CPU のパッケージ温度（℃、複数ソケットなら最も高いもの）。取得できない場合は nil（null）。
	TemperatureC *float64 `json:"temperatureC"`
}

// MemoryMetrics は MemoryMetricsSchema に対応する。
type MemoryMetrics struct {
	TotalBytes uint64 `json:"totalBytes"`
	UsedBytes  uint64 `json:"usedBytes"`
}

// DiskMetrics は DiskMetricsSchema に対応する。
type DiskMetrics struct {
	// Mount はマウントポイント（例: "/", "/data"）。
	Mount      string `json:"mount"`
	TotalBytes uint64 `json:"totalBytes"`
	UsedBytes  uint64 `json:"usedBytes"`
}

// GPUMetrics は GpuMetricsSchema に対応する。収集は nvidia-smi で行う（gpu.go）。
type GPUMetrics struct {
	Index              int     `json:"index"`
	Name               string  `json:"name"`
	UtilizationPercent float64 `json:"utilizationPercent"`
	MemoryTotalBytes   uint64  `json:"memoryTotalBytes"`
	MemoryUsedBytes    uint64  `json:"memoryUsedBytes"`
	// TemperatureC は温度（℃）。取得できない場合は nil。
	TemperatureC *float64 `json:"temperatureC"`
	// PowerW は消費電力（W）。取得できない場合は nil。
	PowerW *float64 `json:"powerW"`
}

// ProcessMetrics は ProcessMetricsSchema に対応する。収集は processes.go で行う。
type ProcessMetrics struct {
	PID  int32  `json:"pid"`
	User string `json:"user"`
	// Name はコマンド名（引数は含めない）。
	Name        string  `json:"name"`
	CPUPercent  float64 `json:"cpuPercent"`
	MemoryBytes uint64  `json:"memoryBytes"`
	// GPUMemoryBytes は GPU メモリの使用量。GPU を使っていない場合は nil。
	GPUMemoryBytes *uint64 `json:"gpuMemoryBytes"`
}

// Collector はリソース情報を収集する。
type Collector interface {
	// Collect は現在のリソース情報を収集して返す。
	// ctx がキャンセルされたら速やかに中断すること。
	Collect(ctx context.Context) (*Snapshot, error)
}
