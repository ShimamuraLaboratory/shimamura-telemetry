package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// fakeSource は呼ばれるたびに cpuTotals・cpuCores を 1 つずつ進めて返す systemSource。
type fakeSource struct {
	cpuTotals  []cpu.TimesStat
	cpuCores   [][]cpu.TimesStat
	totalCalls int
	coreCalls  int
	cpuErr     error

	memory *mem.VirtualMemoryStat
	memErr error

	partitions []disk.PartitionStat
	partErr    error
	usage      map[string]*disk.UsageStat
}

func (f *fakeSource) CPUTimes(_ context.Context, perCPU bool) ([]cpu.TimesStat, error) {
	if f.cpuErr != nil {
		return nil, f.cpuErr
	}
	if perCPU {
		c := f.cpuCores[min(f.coreCalls, len(f.cpuCores)-1)]
		f.coreCalls++
		return c, nil
	}
	t := f.cpuTotals[min(f.totalCalls, len(f.cpuTotals)-1)]
	f.totalCalls++
	return []cpu.TimesStat{t}, nil
}

func (f *fakeSource) VirtualMemory(context.Context) (*mem.VirtualMemoryStat, error) {
	return f.memory, f.memErr
}

func (f *fakeSource) Partitions(context.Context) ([]disk.PartitionStat, error) {
	return f.partitions, f.partErr
}

func (f *fakeSource) DiskUsage(_ context.Context, path string) (*disk.UsageStat, error) {
	u, ok := f.usage[path]
	if !ok {
		return nil, fmt.Errorf("no usage for %s", path)
	}
	return u, nil
}

func newTestSystem(src *fakeSource) *System {
	return &System{
		source:          src,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:             func() time.Time { return time.UnixMilli(1_700_000_000_000) },
		firstSampleWait: 0,
	}
}

func baseSource() *fakeSource {
	return &fakeSource{
		cpuTotals: []cpu.TimesStat{{User: 100, Idle: 100}},
		cpuCores:  [][]cpu.TimesStat{{{User: 50, Idle: 50}}},
		memory:    &mem.VirtualMemoryStat{Total: 1000, Available: 400, Used: 300},
		usage:     map[string]*disk.UsageStat{},
	}
}

func TestSystemCollectCPU(t *testing.T) {
	src := baseSource()
	src.cpuTotals = []cpu.TimesStat{
		{User: 100, System: 50, Idle: 800, Iowait: 50},
		// 差分: busy 30+10+(guest は数えない) = 40、idle 50+10 = 60 → 40%
		{User: 130, System: 60, Idle: 850, Iowait: 60, Guest: 1000},
		// 差分: busy 100、idle 0 → 100%
		{User: 230, System: 60, Idle: 850, Iowait: 60},
	}
	src.cpuCores = [][]cpu.TimesStat{
		{{CPU: "cpu0", User: 10, Idle: 90}, {CPU: "cpu1", User: 10, Idle: 90}},
		{{CPU: "cpu0", User: 20, Idle: 90}, {CPU: "cpu1", User: 10, Idle: 190}},
		// コアが 1 つ増えた
		{{CPU: "cpu0", User: 20, Idle: 100}, {CPU: "cpu1", User: 10, Idle: 190}, {CPU: "cpu2", User: 5, Idle: 5}},
	}
	s := newTestSystem(src)

	snap, err := s.Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Metrics.CPU.UsagePercent; got != 40 {
		t.Errorf("first usage = %v, want 40", got)
	}
	if got := snap.Metrics.CPU.Cores; len(got) != 2 || got[0] != 100 || got[1] != 0 {
		t.Errorf("first cores = %v, want [100 0]", got)
	}

	snap, err = s.Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Metrics.CPU.UsagePercent; got != 100 {
		t.Errorf("second usage = %v, want 100", got)
	}
	if got := snap.Metrics.CPU.Cores; len(got) != 3 || got[0] != 0 || got[1] != 0 || got[2] != 0 {
		t.Errorf("second cores = %v, want [0 0 0]", got)
	}
}

// 途中のコアがオフラインになって並びがずれても、コア名で前回の値と対応付ける。
func TestSystemCollectCPUCoreOffline(t *testing.T) {
	src := baseSource()
	src.cpuCores = [][]cpu.TimesStat{
		{{CPU: "cpu0", User: 0, Idle: 0}, {CPU: "cpu1", User: 1000, Idle: 0}, {CPU: "cpu2", User: 0, Idle: 1000}},
		// cpu1 がオフラインになり、cpu2 が 2 番目に来た
		{{CPU: "cpu0", User: 10, Idle: 10}, {CPU: "cpu2", User: 0, Idle: 1010}},
		// cpu1 がオンラインに戻った（前回の値が無いので 0）
		{{CPU: "cpu0", User: 20, Idle: 20}, {CPU: "cpu1", User: 2000, Idle: 0}, {CPU: "cpu2", User: 10, Idle: 1010}},
	}
	s := newTestSystem(src)

	snap, err := s.Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Metrics.CPU.Cores; len(got) != 2 || got[0] != 50 || got[1] != 0 {
		t.Errorf("first cores = %v, want [50 0]", got)
	}

	snap, err = s.Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Metrics.CPU.Cores; len(got) != 3 || got[0] != 50 || got[1] != 0 || got[2] != 100 {
		t.Errorf("second cores = %v, want [50 0 100]", got)
	}
}

func TestUsagePercent(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur cpu.TimesStat
		want      float64
	}{
		{"no change", cpu.TimesStat{User: 1, Idle: 1}, cpu.TimesStat{User: 1, Idle: 1}, 0},
		{"counter went back", cpu.TimesStat{User: 10, Idle: 10}, cpu.TimesStat{User: 5, Idle: 20}, 0},
		{"rounded", cpu.TimesStat{}, cpu.TimesStat{User: 1, Idle: 2}, 33.33},
		{"steal and irq are busy", cpu.TimesStat{}, cpu.TimesStat{Steal: 1, Irq: 1, Softirq: 1, Nice: 1, Idle: 4}, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usagePercent(tt.prev, tt.cur); got != tt.want {
				t.Errorf("usagePercent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSystemCollectMemory(t *testing.T) {
	tests := []struct {
		name string
		vm   mem.VirtualMemoryStat
		want MemoryMetrics
	}{
		{"total minus available", mem.VirtualMemoryStat{Total: 1000, Available: 400, Used: 300}, MemoryMetrics{1000, 600}},
		{"available larger than total", mem.VirtualMemoryStat{Total: 1000, Available: 2000, Used: 300}, MemoryMetrics{1000, 300}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := baseSource()
			src.memory = &tt.vm
			snap, err := newTestSystem(src).Collect(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if snap.Metrics.Memory != tt.want {
				t.Errorf("memory = %+v, want %+v", snap.Metrics.Memory, tt.want)
			}
		})
	}
}

func TestSystemCollectDisks(t *testing.T) {
	src := baseSource()
	src.partitions = []disk.PartitionStat{
		{Device: "/dev/nvme0n1p2", Mountpoint: "/", Fstype: "ext4"},
		{Device: "/dev/sda1", Mountpoint: "/data", Fstype: "xfs"},
		// bind mount（同じデバイス）は短いパスだけ残す
		{Device: "/dev/sda1", Mountpoint: "/var/lib/docker/volumes/x", Fstype: "xfs"},
		{Device: "/dev/nvme0n1p1", Mountpoint: "/boot/efi", Fstype: "vfat"},
		{Device: "/dev/loop3", Mountpoint: "/snap/core/1", Fstype: "squashfs"},
		{Device: "/dev/loop4", Mountpoint: "/mnt/img", Fstype: "ext4"},
		{Device: "tmpfs", Mountpoint: "/run", Fstype: "tmpfs"},
		{Device: "overlay", Mountpoint: "/var/lib/docker/overlay2/x/merged", Fstype: "overlay"},
		{Device: "nas:/export", Mountpoint: "/mnt/nas", Fstype: "nfs4"},
		{Device: "sshfs#host:", Mountpoint: "/mnt/ssh", Fstype: "fuse.sshfs"},
		{Device: "/dev/sdb1", Mountpoint: "/" + strings.Repeat("a", MaxMountLength), Fstype: "ext4"},
		// 読めないマウントは飛ばす
		{Device: "/dev/sdc1", Mountpoint: "/broken", Fstype: "ext4"},
		// 総量 0 は飛ばす
		{Device: "/dev/sdd1", Mountpoint: "/empty", Fstype: "ext4"},
	}
	src.usage = map[string]*disk.UsageStat{
		"/":                         {Total: 500, Used: 100},
		"/data":                     {Total: 2000, Used: 1500},
		"/var/lib/docker/volumes/x": {Total: 2000, Used: 1500},
		"/boot/efi":                 {Total: 50, Used: 60},
		"/empty":                    {Total: 0, Used: 0},
	}

	snap, err := newTestSystem(src).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []DiskMetrics{
		{Mount: "/", TotalBytes: 500, UsedBytes: 100},
		{Mount: "/data", TotalBytes: 2000, UsedBytes: 1500},
		// 使用量は総量を超えない
		{Mount: "/boot/efi", TotalBytes: 50, UsedBytes: 50},
	}
	if fmt.Sprint(snap.Metrics.Disks) != fmt.Sprint(want) {
		t.Errorf("disks = %+v, want %+v", snap.Metrics.Disks, want)
	}
}

func TestSystemCollectDisksLimit(t *testing.T) {
	src := baseSource()
	for i := range MaxDisks + 10 {
		mount := fmt.Sprintf("/mnt/d%03d", i)
		src.partitions = append(src.partitions, disk.PartitionStat{Device: fmt.Sprintf("/dev/d%d", i), Mountpoint: mount, Fstype: "ext4"})
		src.usage[mount] = &disk.UsageStat{Total: 1, Used: 1}
	}
	snap, err := newTestSystem(src).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(snap.Metrics.Disks); got != MaxDisks {
		t.Errorf("len(disks) = %d, want %d", got, MaxDisks)
	}
}

func TestSystemCollectErrors(t *testing.T) {
	t.Run("cpu error", func(t *testing.T) {
		src := baseSource()
		src.cpuErr = errors.New("boom")
		if _, err := newTestSystem(src).Collect(t.Context()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("memory error", func(t *testing.T) {
		src := baseSource()
		src.memErr = errors.New("boom")
		if _, err := newTestSystem(src).Collect(t.Context()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("partitions error returns empty disks", func(t *testing.T) {
		src := baseSource()
		src.partErr = errors.New("boom")
		snap, err := newTestSystem(src).Collect(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if snap.Metrics.Disks == nil || len(snap.Metrics.Disks) != 0 {
			t.Errorf("disks = %#v, want empty non-nil slice", snap.Metrics.Disks)
		}
	})
	t.Run("canceled during first sample", func(t *testing.T) {
		s := newTestSystem(baseSource())
		s.firstSampleWait = time.Hour
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := s.Collect(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// JSON のキー名と、空の配列が null にならないことを確認する（監視 Worker の検証に通るため）。
func TestSnapshotJSON(t *testing.T) {
	snap, err := newTestSystem(baseSource()).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(snap.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cpu", "memory", "disks", "gpus"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q in %s", key, b)
		}
	}
	if _, ok := got["processes"]; ok {
		t.Errorf("processes should be omitted: %s", b)
	}
	if got["disks"] == nil || got["gpus"] == nil {
		t.Errorf("disks and gpus must not be null: %s", b)
	}
	cpuJSON := got["cpu"].(map[string]any)
	for _, key := range []string{"usagePercent", "cores"} {
		if _, ok := cpuJSON[key]; !ok {
			t.Errorf("missing key cpu.%s in %s", key, b)
		}
	}
	memJSON := got["memory"].(map[string]any)
	for _, key := range []string{"totalBytes", "usedBytes"} {
		if _, ok := memJSON[key]; !ok {
			t.Errorf("missing key memory.%s in %s", key, b)
		}
	}
}

// 実際の OS から読めることを確認する（値の正しさは見ない）。
func TestNewSystemCollectsFromOS(t *testing.T) {
	snap, err := NewSystem(nil).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	m := snap.Metrics
	if len(m.CPU.Cores) == 0 {
		t.Error("no cpu cores")
	}
	if m.CPU.UsagePercent < 0 || m.CPU.UsagePercent > 100 {
		t.Errorf("cpu usage = %v", m.CPU.UsagePercent)
	}
	if m.Memory.TotalBytes == 0 || m.Memory.UsedBytes > m.Memory.TotalBytes {
		t.Errorf("memory = %+v", m.Memory)
	}
	// コンテナ内（overlay・tmpfs・bind mount だけ）ではディスクが空になるのが正しいため、件数は確認しない。
	// ディスクの絞り込みは fakeSource を使ったテストで確認している。
	for _, d := range m.Disks {
		if d.TotalBytes == 0 || d.UsedBytes > d.TotalBytes {
			t.Errorf("disk = %+v", d)
		}
	}
}
