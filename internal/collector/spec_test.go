package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// writeTopology は sysfs の CPU トポロジー（cpuN/topology/{physical_package_id,core_id}）を作る。
func writeTopology(t *testing.T, root string, cpus map[int][2]int) {
	t.Helper()
	for n, ids := range cpus {
		dir := filepath.Join(root, "devices/system/cpu", fmt.Sprintf("cpu%d", n), "topology")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]int{"physical_package_id": ids[0], "core_id": ids[1]} {
			if err := os.WriteFile(filepath.Join(dir, name), fmt.Appendf(nil, "%d\n", v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCPUTopologyMultiSocket(t *testing.T) {
	root := t.TempDir()
	// 2 ソケット × 4 コア × 2 スレッド（SMT）
	cpus := map[int][2]int{}
	n := 0
	for pkg := range 2 {
		for thread := range 2 {
			for core := range 4 {
				cpus[n] = [2]int{pkg, core}
				n++
				_ = thread
			}
		}
	}
	writeTopology(t, root, cpus)
	// オフラインの CPU（topology が無い）は数えない
	if err := os.MkdirAll(filepath.Join(root, "devices/system/cpu/cpu99"), 0o755); err != nil {
		t.Fatal(err)
	}
	// cpufreq など cpuN 以外のディレクトリも数えない
	if err := os.MkdirAll(filepath.Join(root, "devices/system/cpu/cpufreq"), 0o755); err != nil {
		t.Fatal(err)
	}

	sockets, cores, threads, err := cpuTopology(root)
	if err != nil {
		t.Fatal(err)
	}
	if sockets != 2 || cores != 8 || threads != 16 {
		t.Errorf("topology = %d sockets, %d cores, %d threads; want 2, 8, 16", sockets, cores, threads)
	}

	if _, _, _, err := cpuTopology(t.TempDir()); err == nil {
		t.Error("expected error for empty sysfs")
	}
}

func TestCountFromCPUInfo(t *testing.T) {
	// 2 ソケット × 2 コア × 2 スレッド
	var infos []cpu.InfoStat
	n := int32(0)
	for _, pkg := range []string{"0", "1"} {
		for range 2 {
			for _, core := range []string{"0", "1"} {
				infos = append(infos, cpu.InfoStat{CPU: n, PhysicalID: pkg, CoreID: core})
				n++
			}
		}
	}
	if s, c, th := countFromCPUInfo(infos); s != 2 || c != 4 || th != 8 {
		t.Errorf("x86 = %d, %d, %d; want 2, 4, 8", s, c, th)
	}

	// 全体を 1 件にまとめて返す環境（macOS）
	if s, c, th := countFromCPUInfo([]cpu.InfoStat{{Cores: 2}}); s != 1 || c != 2 || th < 2 {
		t.Errorf("aggregated = %d, %d, %d", s, c, th)
	}

	// physical id・core id の無い環境（一部の ARM）は 1 ソケット、論理 CPU 数をコア数とする
	arm := []cpu.InfoStat{{CPU: 0}, {CPU: 1}, {CPU: 2}, {CPU: 3}}
	if s, c, th := countFromCPUInfo(arm); s != 1 || c != 4 || th != 4 {
		t.Errorf("arm = %d, %d, %d; want 1, 4, 4", s, c, th)
	}
}

func TestCPUModel(t *testing.T) {
	infos := []cpu.InfoStat{{ModelName: ""}, {ModelName: "  AMD EPYC 7543   32-Core Processor \n"}}
	if got := cpuModel(infos); got != "AMD EPYC 7543 32-Core Processor" {
		t.Errorf("model = %q", got)
	}
	if got := cpuModel(nil); got != "" {
		t.Errorf("model = %q, want empty", got)
	}
	long := strings.Repeat("x", MaxCPUModelLength+10)
	if got := cpuModel([]cpu.InfoStat{{ModelName: long}}); len(got) != MaxCPUModelLength {
		t.Errorf("model length = %d", len(got))
	}
}

func newSpecSystem(t *testing.T) (*System, *fakeSource, *fakeGPUReader, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	writeTopology(t, root, map[int][2]int{0: {0, 0}, 1: {0, 0}, 2: {0, 1}, 3: {0, 1}})
	src := baseSource()
	src.cpuInfo = []cpu.InfoStat{{ModelName: "Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz"}}
	src.memory = &mem.VirtualMemoryStat{Total: 540431343616, Available: 1}
	src.partitions = []disk.PartitionStat{
		{Device: "/dev/nvme0n1p2", Mountpoint: "/", Fstype: "ext4"},
		{Device: "/dev/nvme0n1p2", Mountpoint: "/var/lib/docker", Fstype: "ext4"},
		{Device: "tmpfs", Mountpoint: "/run", Fstype: "tmpfs"},
		{Device: "/dev/sda1", Mountpoint: "/data", Fstype: "xfs"},
	}
	src.usage = map[string]*disk.UsageStat{
		"/":     {Total: 3840755982336, Used: 1},
		"/data": {Total: 8000000000000, Used: 1},
	}
	gpu := &fakeGPUReader{gpus: []GPUMetrics{
		{Index: 0, Name: "NVIDIA RTX A6000", MemoryTotalBytes: 51527024640, MemoryUsedBytes: 10},
		{Index: 1, Name: "NVIDIA RTX A6000", MemoryTotalBytes: 51527024640},
	}}
	var logs bytes.Buffer
	s := newTestSystem(src)
	s.gpu = gpu
	s.sysRoot = root
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))
	return s, src, gpu, &logs
}

func TestSystemSpec(t *testing.T) {
	s, _, _, _ := newSpecSystem(t)
	spec, err := s.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(spec)
	want := `{"cpu":{"model":"Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz","sockets":1,"cores":2,"threads":4},` +
		`"memoryTotalBytes":540431343616,` +
		`"disks":[{"mount":"/","totalBytes":3840755982336},{"mount":"/data","totalBytes":8000000000000}],` +
		`"gpus":[{"index":0,"name":"NVIDIA RTX A6000","memoryTotalBytes":51527024640},{"index":1,"name":"NVIDIA RTX A6000","memoryTotalBytes":51527024640}]}`
	if string(b) != want {
		t.Errorf("spec =\n%s\nwant\n%s", b, want)
	}
}

func TestSystemSpecWithoutGPU(t *testing.T) {
	s, _, gpu, logs := newSpecSystem(t)
	gpu.gpus, gpu.err = nil, errNoNvidiaSMI
	spec, err := s.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(spec.GPUs); string(b) != "[]" {
		t.Errorf("gpus = %s, want []", b)
	}
	// GPU が無いのは異常ではないので警告しない
	if strings.Contains(logs.String(), "WARN") {
		t.Errorf("logs = %s", logs.String())
	}

	// 一度も読めていないのに nvidia-smi が失敗した場合は空で送り、警告を出す
	gpu.err = errors.New("Failed to initialize NVML")
	spec, err = s.Spec(t.Context())
	if err != nil || len(spec.GPUs) != 0 || spec.GPUs == nil {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	if !strings.Contains(logs.String(), "failed to read gpu spec") {
		t.Errorf("logs = %s", logs.String())
	}

	// GPU を読まない（gpu が nil）場合も空配列
	s.gpu = nil
	spec, _ = s.Spec(t.Context())
	if spec.GPUs == nil {
		t.Error("gpus must not be nil")
	}
}

func TestSystemSpecPartialFailures(t *testing.T) {
	s, src, _, _ := newSpecSystem(t)
	// sysfs が読めない → /proc/cpuinfo から数える
	s.sysRoot = t.TempDir()
	src.cpuInfo = []cpu.InfoStat{
		{CPU: 0, ModelName: "m", PhysicalID: "0", CoreID: "0"},
		{CPU: 1, ModelName: "m", PhysicalID: "1", CoreID: "0"},
	}
	src.memErr = errors.New("boom")
	src.partErr = errors.New("boom")

	spec, err := s.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if spec.CPU != (CPUSpec{Model: "m", Sockets: 2, Cores: 2, Threads: 2}) {
		t.Errorf("cpu = %+v", spec.CPU)
	}
	if spec.MemoryTotalBytes != 0 || spec.Disks == nil || len(spec.Disks) != 0 {
		t.Errorf("spec = %+v", spec)
	}
	if len(spec.GPUs) != 2 {
		t.Errorf("gpus = %+v", spec.GPUs)
	}

	// CPU の情報がどちらも読めない → モデル名は空、数は 0（取得できた範囲で送る）
	src.cpuInfo, src.cpuInfoErr = nil, errors.New("no cpuinfo")
	spec, err = s.Spec(t.Context())
	if err != nil || spec.CPU != (CPUSpec{}) {
		t.Errorf("cpu = %+v, %v", spec.CPU, err)
	}
}

// 実際の OS から読めることを確認する（値の正しさは見ない）。
func TestNewSystemSpecFromOS(t *testing.T) {
	spec, err := NewSystem(slog.New(slog.DiscardHandler)).Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if spec.CPU.Threads <= 0 || spec.CPU.Cores <= 0 || spec.CPU.Sockets <= 0 || spec.MemoryTotalBytes == 0 {
		t.Errorf("spec = %+v", spec)
	}
	if spec.CPU.Cores > spec.CPU.Threads {
		t.Errorf("cores %d > threads %d", spec.CPU.Cores, spec.CPU.Threads)
	}
}

// nvidia-smi が一時的に失敗しても、前回読めた GPU の一覧で送る（「GPU なし」と送らない）。
func TestSystemSpecKeepsGPUsOnTransientError(t *testing.T) {
	s, _, gpu, _ := newSpecSystem(t)
	if spec, _ := s.Spec(t.Context()); len(spec.GPUs) != 2 {
		t.Fatalf("gpus = %+v", spec.GPUs)
	}
	gpu.err = errors.New("Failed to initialize NVML")
	spec, err := s.Spec(t.Context())
	if err != nil || len(spec.GPUs) != 2 || spec.GPUs[0].Name != "NVIDIA RTX A6000" {
		t.Fatalf("gpus = %+v, %v", spec.GPUs, err)
	}

	// ドライバが外された（GPU が無くなった）場合は空にする
	gpu.err = errNoNvidiaSMI
	if spec, _ := s.Spec(t.Context()); len(spec.GPUs) != 0 {
		t.Errorf("gpus = %+v", spec.GPUs)
	}
}
