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

func (gopsutilSource) DiskUsage(ctx context.Context, path string) (*disk.UsageStat, error) {
	return disk.UsageWithContext(ctx, path)
}

// System は gopsutil で CPU・メモリ・SSD の使用状況を収集する Collector。
// GPU（Issue #3）とプロセス（Issue #5）は収集せず、GPUs は空、Processes は nil で返す。
type System struct {
	source          systemSource
	logger          *slog.Logger
	now             func() time.Time
	firstSampleWait time.Duration

	mu sync.Mutex
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
		logger:          logger,
		now:             time.Now,
		firstSampleWait: defaultFirstSampleWait,
	}
}

// Collect は CPU・メモリ・SSD の使用状況を収集する。
// CPU・メモリが読めない場合はエラーを返す。SSD が読めない場合はログに残して空で返す。
func (s *System) Collect(ctx context.Context) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cpuMetrics, err := s.collectCPU(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect cpu: %w", err)
	}
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

	return &Snapshot{
		CollectedAt: s.now(),
		Metrics: Metrics{
			CPU:    cpuMetrics,
			Memory: memory,
			Disks:  disks,
			GPUs:   []GPUMetrics{},
		},
	}, nil
}

func (s *System) collectCPU(ctx context.Context) (CPUMetrics, error) {
	if s.prevTotal == nil {
		// 初回は基準となる累積時間を読んでから少し待つ
		if err := s.readCPUTimes(ctx); err != nil {
			return CPUMetrics{}, err
		}
		if err := sleep(ctx, s.firstSampleWait); err != nil {
			return CPUMetrics{}, err
		}
	}
	prevTotal, prevCores := *s.prevTotal, s.prevCores
	if err := s.readCPUTimes(ctx); err != nil {
		return CPUMetrics{}, err
	}

	cores := make([]float64, 0, min(len(s.prevCores), MaxCores))
	for i, cur := range s.prevCores {
		if i >= MaxCores {
			break
		}
		// CPU のホットプラグなどで数が変わった場合、前回の無いコアは 0 とする
		if i >= len(prevCores) {
			cores = append(cores, 0)
			continue
		}
		cores = append(cores, usagePercent(prevCores[i], cur))
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
