package collector

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/sensors"
)

// defaultFirstSampleWait は初回の Collect で CPU 使用率を求めるために、2 回目の計測までに待つ時間。
// CPU 使用率は 2 時点の累積時間の差から求めるため、初回だけは基準となる計測が必要になる。
const defaultFirstSampleWait = 500 * time.Millisecond

// excludedFSTypes は使用量を送らないファイルシステム。
// メモリ上のもの・読み取り専用のイメージ（snap の squashfs など）・コンテナの overlay、
// それに応答しないと statfs が止まりうるネットワークファイルシステムを除く。
var excludedFSTypes = map[string]bool{
	"autofs":   true,
	"cifs":     true,
	"devfs":    true,
	"devtmpfs": true,
	"iso9660":  true,
	"nfs":      true,
	"nfs4":     true,
	"overlay":  true,
	"ramfs":    true,
	"smbfs":    true,
	"squashfs": true,
	"tmpfs":    true,
	"udf":      true,
}

// systemSource は OS からリソース情報を読む処理。テストで差し替えるために分けている。
type systemSource interface {
	CPUTimes(ctx context.Context, perCPU bool) ([]cpu.TimesStat, error)
	VirtualMemory(ctx context.Context) (*mem.VirtualMemoryStat, error)
	Partitions(ctx context.Context) ([]disk.PartitionStat, error)
	DiskUsage(ctx context.Context, path string) (*disk.UsageStat, error)
	// CPUInfo は論理 CPU ごとの情報（/proc/cpuinfo）を読む。ハードウェア情報（Spec）で使う。
	CPUInfo(ctx context.Context) ([]cpu.InfoStat, error)
	// Temperatures は温度センサー（/sys/class/hwmon）の値を読む。CPU の温度に使う。
	Temperatures(ctx context.Context) ([]sensors.TemperatureStat, error)
}

// gopsutilSource は gopsutil で OS から読む systemSource。
type gopsutilSource struct{}

func (gopsutilSource) CPUTimes(ctx context.Context, perCPU bool) ([]cpu.TimesStat, error) {
	return cpu.TimesWithContext(ctx, perCPU)
}

func (gopsutilSource) VirtualMemory(ctx context.Context) (*mem.VirtualMemoryStat, error) {
	return mem.VirtualMemoryWithContext(ctx)
}

func (gopsutilSource) Partitions(ctx context.Context) ([]disk.PartitionStat, error) {
	// all=false で、デバイスを持たない擬似ファイルシステム（proc・sysfs・cgroup など）を除く
	return disk.PartitionsWithContext(ctx, false)
}

func (gopsutilSource) Temperatures(ctx context.Context) ([]sensors.TemperatureStat, error) {
	return sensors.TemperaturesWithContext(ctx)
}

func (gopsutilSource) CPUInfo(ctx context.Context) ([]cpu.InfoStat, error) {
	return cpu.InfoWithContext(ctx)
}

func (gopsutilSource) DiskUsage(ctx context.Context, path string) (*disk.UsageStat, error) {
	return disk.UsageWithContext(ctx, path)
}

// System は CPU・メモリ・SSD・プロセス（gopsutil）と GPU（nvidia-smi）の使用状況を収集する Collector。
type System struct {
	source systemSource
	gpu    gpuReader
	// processes が nil のときはプロセスを収集しない（Processes は nil で送らない）。
	processes       *processCollector
	logger          *slog.Logger
	now             func() time.Time
	firstSampleWait time.Duration
	// sysRoot は sysfs の場所（CPU のトポロジーを読む）。テストで差し替えるために持つ。
	sysRoot string

	mu sync.Mutex
	// gpuState は前回の GPU の読み取り結果。状態が変わったときだけログに出すために使う。
	gpuState gpuState
	// lastGPUSpec は前回 Spec で読めた GPU の一覧。nvidia-smi が一時的に失敗したときに使う。
	lastGPUSpec []GPUSpec
	// prevTotal・prevCores は前回の Collect で読んだ CPU の累積時間。初回は nil。
	prevTotal *cpu.TimesStat
	prevCores []cpu.TimesStat
}

var _ Collector = (*System)(nil)

// NewSystem は System を作る。logger が nil のときは slog.Default() を使う。
func NewSystem(logger *slog.Logger) *System {
	if logger == nil {
		logger = slog.Default()
	}
	return &System{
		source:          gopsutilSource{},
		gpu:             newNvidiaSMI(),
		processes:       newProcessCollector(time.Now),
		logger:          logger,
		now:             time.Now,
		firstSampleWait: defaultFirstSampleWait,
		sysRoot:         "/sys",
	}
}

// Collect は CPU・メモリ・SSD・GPU・プロセスの使用状況を収集する。
// CPU・メモリが読めない場合はエラーを返す。SSD・GPU・プロセスが読めない場合はログに残して空で返す
// （GPU の無いサーバーや、ドライバが応答しない場合でも CPU・メモリは送り続けるため）。
func (s *System) Collect(ctx context.Context) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cpuMetrics, err := s.collectCPU(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect cpu: %w", err)
	}
	cpuMetrics.TemperatureC = s.collectCPUTemperature(ctx)
	memory, err := s.collectMemory(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect memory: %w", err)
	}
	disks, err := s.collectDisks(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		s.logger.Warn("failed to collect disks", "error", err)
		disks = []DiskMetrics{}
	}
	gpus, gpuProcesses, err := s.collectGPUs(ctx)
	if err != nil {
		return nil, err
	}
	processes, err := s.collectProcesses(ctx, gpuProcesses)
	if err != nil {
		return nil, err
	}

	return &Snapshot{
		CollectedAt: s.now(),
		Metrics: Metrics{
			CPU:       cpuMetrics,
			Memory:    memory,
			Disks:     disks,
			GPUs:      gpus,
			Processes: processes,
		},
	}, nil
}

// collectProcesses はプロセスを読む。ctx のキャンセル以外のエラーは nil（送らない）にして返す。
func (s *System) collectProcesses(ctx context.Context, gpuMemory map[int32]uint64) ([]ProcessMetrics, error) {
	if s.processes == nil {
		return nil, nil
	}
	processes, err := s.processes.collect(ctx, gpuMemory)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		s.logger.Warn("failed to collect processes", "error", err)
		return nil, nil
	}
	return processes, nil
}

// collectCPUTemperature は CPU の温度を読む。読めない・CPU のセンサーが無い（VM など）場合は nil。
// 一部のセンサーが読めなくても（gopsutil は読めた分と警告を返す）、読めた分から選ぶ。
func (s *System) collectCPUTemperature(ctx context.Context) *float64 {
	stats, err := s.source.Temperatures(ctx)
	if err != nil && len(stats) == 0 {
		s.logger.Debug("failed to read temperature sensors", "error", err)
		return nil
	}
	return cpuTemperature(stats)
}

// gpuState は GPU の読み取り結果の状態。
type gpuState int

const (
	gpuStateUnknown gpuState = iota
	gpuStateOK
	gpuStateNoDriver
	gpuStateError
)

// collectGPUs は GPU の情報と、GPU を使っているプロセス（PID ごとの GPU メモリ）を読む。
// ctx のキャンセル以外のエラーは空にして返す。
// 5 秒ごとに同じログが出続けないよう、状態が変わったときだけログに出す。
func (s *System) collectGPUs(ctx context.Context) ([]GPUMetrics, map[int32]uint64, error) {
	if s.gpu == nil {
		return []GPUMetrics{}, nil, nil
	}
	gpus, err := s.gpu.ReadGPUs(ctx)
	switch {
	case err == nil:
		if s.gpuState != gpuStateOK {
			s.logger.Info("collecting gpu metrics", "gpus", len(gpus))
		}
		s.gpuState = gpuStateOK
		if s.processes == nil || len(gpus) == 0 {
			return gpus, nil, nil
		}
		procs, err := s.gpu.ReadGPUProcesses(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, nil, ctxErr
			}
			s.logger.Debug("failed to collect gpu processes", "error", err)
			return gpus, nil, nil
		}
		return gpus, procs, nil
	case ctx.Err() != nil:
		return nil, nil, ctx.Err()
	case errors.Is(err, errNoNvidiaSMI):
		if s.gpuState != gpuStateNoDriver {
			s.logger.Info("nvidia-smi not found; gpu metrics are disabled")
		}
		s.gpuState = gpuStateNoDriver
	default:
		if s.gpuState != gpuStateError {
			s.logger.Warn("failed to collect gpus", "error", err)
		}
		s.gpuState = gpuStateError
	}
	return []GPUMetrics{}, nil, nil
}

func (s *System) collectCPU(ctx context.Context) (CPUMetrics, error) {
	if s.prevTotal == nil {
		// 初回は基準となる累積時間を読んでから少し待つ。プロセスの CPU 使用率も同じく 2 時点の差から求めるため、
		// ここで基準を読んでおく（読めなくても次回から求められるので無視する）
		if err := s.readCPUTimes(ctx); err != nil {
			return CPUMetrics{}, err
		}
		if s.processes != nil {
			_ = s.processes.baseline(ctx)
		}
		if err := sleep(ctx, s.firstSampleWait); err != nil {
			return CPUMetrics{}, err
		}
	}
	prevTotal, prevCores := *s.prevTotal, s.prevCores
	if err := s.readCPUTimes(ctx); err != nil {
		return CPUMetrics{}, err
	}

	// CPU のオフライン・オンラインで並びがずれることがあるため、前回の値はコア名（"cpu3" など）で対応付ける。
	// 前回の値が無いコアは 0 とする
	prevByName := make(map[string]cpu.TimesStat, len(prevCores))
	for _, p := range prevCores {
		prevByName[p.CPU] = p
	}
	cores := make([]float64, 0, min(len(s.prevCores), MaxCores))
	for _, cur := range s.prevCores {
		if len(cores) >= MaxCores {
			break
		}
		prev, ok := prevByName[cur.CPU]
		if !ok {
			cores = append(cores, 0)
			continue
		}
		cores = append(cores, usagePercent(prev, cur))
	}
	return CPUMetrics{
		UsagePercent: usagePercent(prevTotal, *s.prevTotal),
		Cores:        cores,
	}, nil
}

// readCPUTimes は全体とコアごとの累積時間を読み、prevTotal・prevCores を更新する。
func (s *System) readCPUTimes(ctx context.Context) error {
	total, err := s.source.CPUTimes(ctx, false)
	if err != nil {
		return err
	}
	if len(total) == 0 {
		return errors.New("no cpu times")
	}
	cores, err := s.source.CPUTimes(ctx, true)
	if err != nil {
		return err
	}
	s.prevTotal = &total[0]
	s.prevCores = cores
	return nil
}

func (s *System) collectMemory(ctx context.Context) (MemoryMetrics, error) {
	vm, err := s.source.VirtualMemory(ctx)
	if err != nil {
		return MemoryMetrics{}, err
	}
	// free コマンドと同じく「総量 − 利用可能量」を使用量とする（ページキャッシュなどは使用量に含めない）
	used := vm.Used
	if vm.Available <= vm.Total {
		used = vm.Total - vm.Available
	}
	return MemoryMetrics{TotalBytes: vm.Total, UsedBytes: used}, nil
}

func (s *System) collectDisks(ctx context.Context) ([]DiskMetrics, error) {
	partitions, err := s.source.Partitions(ctx)
	if err != nil {
		return nil, err
	}

	// 同じデバイスが複数の場所にマウントされている（bind mount など）場合は、最も短いパスだけを残す
	byDevice := map[string]disk.PartitionStat{}
	for _, p := range partitions {
		if !includePartition(p) {
			continue
		}
		key := p.Device
		if key == "" || key == "none" {
			key = p.Mountpoint
		}
		if cur, ok := byDevice[key]; ok && !shorterMount(p.Mountpoint, cur.Mountpoint) {
			continue
		}
		byDevice[key] = p
	}
	mounts := make([]string, 0, len(byDevice))
	for _, p := range byDevice {
		mounts = append(mounts, p.Mountpoint)
	}
	slices.SortFunc(mounts, compareMount)

	disks := make([]DiskMetrics, 0, min(len(mounts), MaxDisks))
	for _, mount := range mounts {
		if len(disks) >= MaxDisks {
			break
		}
		usage, err := s.source.DiskUsage(ctx, mount)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			s.logger.Debug("failed to read disk usage", "mount", mount, "error", err)
			continue
		}
		if usage.Total == 0 {
			continue
		}
		disks = append(disks, DiskMetrics{
			Mount:      mount,
			TotalBytes: usage.Total,
			UsedBytes:  min(usage.Used, usage.Total),
		})
	}
	return disks, nil
}

// includePartition は使用量を送るパーティションかを判定する。
func includePartition(p disk.PartitionStat) bool {
	if p.Mountpoint == "" || utf8.RuneCountInString(p.Mountpoint) > MaxMountLength {
		return false
	}
	fstype := strings.ToLower(p.Fstype)
	if excludedFSTypes[fstype] || strings.HasPrefix(fstype, "fuse.") {
		return false
	}
	// snap などのループバックデバイス
	return !strings.HasPrefix(p.Device, "/dev/loop")
}

// shorterMount は a が b より優先されるマウントポイントかを返す（短いもの、同じ長さなら辞書順で前のもの）。
func shorterMount(a, b string) bool {
	return compareMount(a, b) < 0
}

func compareMount(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}

// usagePercent は 2 時点の累積時間から、その間の使用率（0〜100）を求める。
func usagePercent(prev, cur cpu.TimesStat) float64 {
	// Linux の user・nice には guest・guest_nice が含まれているため、二重に数えないよう足さない
	busy := func(t cpu.TimesStat) float64 {
		return t.User + t.Nice + t.System + t.Irq + t.Softirq + t.Steal
	}
	idle := func(t cpu.TimesStat) float64 {
		return t.Idle + t.Iowait
	}
	dBusy := busy(cur) - busy(prev)
	dTotal := dBusy + idle(cur) - idle(prev)
	if dTotal <= 0 || dBusy <= 0 {
		return 0
	}
	return round2(min(dBusy/dTotal*100, 100))
}

// round2 は小数第 2 位までに丸める（メッセージを小さくするため）。
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
