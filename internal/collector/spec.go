package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"
)

// ハードウェア情報（spec メッセージ）。IdP の管理画面でサーバーを追加するときに手で入力していた
// CPU・メモリ容量・SSD 容量・GPU を、エージェントが接続したときに送る（Issue #17）。

// MaxCPUModelLength は CPU のモデル名の文字数の上限。
const MaxCPUModelLength = 255

// Spec はサーバーのハードウェア情報。
type Spec struct {
	CPU CPUSpec `json:"cpu"`
	// MemoryTotalBytes は物理メモリの合計（バイト）。
	MemoryTotalBytes uint64 `json:"memoryTotalBytes"`
	// Disks はローカルのストレージ（メトリクスの disks と同じマウントポイント）。null にならないよう空でもスライスを入れる。
	Disks []DiskSpec `json:"disks"`
	// GPUs は NVIDIA GPU。無ければ空。null にならないよう空でもスライスを入れる。
	GPUs []GPUSpec `json:"gpus"`
}

// CPUSpec は CPU の構成。
type CPUSpec struct {
	// Model はモデル名（/proc/cpuinfo の model name）。取得できなければ空文字。
	Model string `json:"model"`
	// Sockets は物理 CPU（ソケット）の数。
	Sockets int `json:"sockets"`
	// Cores は物理コア数の合計。
	Cores int `json:"cores"`
	// Threads は論理 CPU 数の合計。
	Threads int `json:"threads"`
}

// DiskSpec はストレージ 1 つの容量。
type DiskSpec struct {
	Mount      string `json:"mount"`
	TotalBytes uint64 `json:"totalBytes"`
}

// GPUSpec は GPU 1 枚の情報。
type GPUSpec struct {
	Index            int    `json:"index"`
	Name             string `json:"name"`
	MemoryTotalBytes uint64 `json:"memoryTotalBytes"`
}

// Spec はハードウェア情報を読む。取得に失敗した項目があっても、取得できた範囲で返す
// （CPU のモデル名が取れなければ空文字、GPU が読めなければ空）。ctx がキャンセルされた場合だけエラーを返す。
func (s *System) Spec(ctx context.Context) (Spec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	spec := Spec{Disks: []DiskSpec{}, GPUs: []GPUSpec{}}

	cpuSpec, err := s.cpuSpec(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Spec{}, ctxErr
		}
		s.logger.Warn("failed to read cpu spec", "error", err)
	}
	spec.CPU = cpuSpec

	if vm, err := s.source.VirtualMemory(ctx); err == nil {
		spec.MemoryTotalBytes = vm.Total
	} else {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Spec{}, ctxErr
		}
		s.logger.Warn("failed to read memory spec", "error", err)
	}

	disks, err := s.collectDisks(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Spec{}, ctxErr
		}
		s.logger.Warn("failed to read disk spec", "error", err)
	}
	for _, d := range disks {
		spec.Disks = append(spec.Disks, DiskSpec{Mount: d.Mount, TotalBytes: d.TotalBytes})
	}

	if s.gpu != nil {
		gpus, err := s.gpu.ReadGPUs(ctx)
		switch {
		case err == nil:
			for _, g := range gpus {
				spec.GPUs = append(spec.GPUs, GPUSpec{Index: g.Index, Name: g.Name, MemoryTotalBytes: g.MemoryTotalBytes})
			}
			s.lastGPUSpec = spec.GPUs
		case ctx.Err() != nil:
			return Spec{}, ctx.Err()
		case errors.Is(err, errNoNvidiaSMI):
			// ドライバが無い（GPU が無い）
			s.lastGPUSpec = spec.GPUs
		default:
			// nvidia-smi が一時的に失敗しただけで「GPU なし」と送らないよう、前回読めた一覧を使う
			s.logger.Warn("failed to read gpu spec", "error", err)
			if s.lastGPUSpec != nil {
				spec.GPUs = s.lastGPUSpec
			}
		}
	}
	return spec, nil
}

// cpuSpec は CPU のモデル名と構成を読む。ソケット・コア・スレッドの数は sysfs のトポロジーから数え、
// 読めない場合は /proc/cpuinfo（gopsutil の cpu.Info）から数える。
func (s *System) cpuSpec(ctx context.Context) (CPUSpec, error) {
	infos, infoErr := s.source.CPUInfo(ctx)
	spec := CPUSpec{Model: cpuModel(infos)}

	sockets, cores, threads, err := cpuTopology(s.sysRoot)
	if err == nil {
		spec.Sockets, spec.Cores, spec.Threads = sockets, cores, threads
		return spec, nil
	}
	if infoErr != nil {
		return spec, errors.Join(fmt.Errorf("read cpu topology: %w", err), infoErr)
	}
	spec.Sockets, spec.Cores, spec.Threads = countFromCPUInfo(infos)
	return spec, nil
}

// cpuModel は最初に見つかったモデル名を返す（前後の空白は除く）。
func cpuModel(infos []cpu.InfoStat) string {
	for _, info := range infos {
		if m := strings.Join(strings.Fields(info.ModelName), " "); m != "" {
			return truncateRunes(m, MaxCPUModelLength)
		}
	}
	return ""
}

// cpuTopology は sysfs（<root>/devices/system/cpu/cpuN/topology）から、ソケット・物理コア・論理 CPU の数を数える。
// オンラインの CPU（topology がある CPU）だけを数える。
func cpuTopology(root string) (sockets, cores, threads int, err error) {
	dirs, err := filepath.Glob(filepath.Join(root, "devices/system/cpu/cpu[0-9]*"))
	if err != nil {
		return 0, 0, 0, err
	}
	socketSet := map[string]bool{}
	coreSet := map[string]bool{}
	for _, dir := range dirs {
		pkg, err := readTrimmed(filepath.Join(dir, "topology/physical_package_id"))
		if err != nil {
			continue
		}
		core, err := readTrimmed(filepath.Join(dir, "topology/core_id"))
		if err != nil {
			continue
		}
		threads++
		socketSet[pkg] = true
		coreSet[pkg+"/"+core] = true
	}
	if threads == 0 {
		return 0, 0, 0, fmt.Errorf("no cpu topology under %s", root)
	}
	return len(socketSet), len(coreSet), threads, nil
}

// countFromCPUInfo は /proc/cpuinfo の各論理 CPU（physical id・core id）から数を数える。
// physical id が無い環境（一部の ARM など）は 1 ソケット、core id が無ければ論理 CPU 数をコア数とする。
// macOS のように全体を 1 件にまとめて返す環境では、その件のコア数（Cores）と論理 CPU 数を使う。
func countFromCPUInfo(infos []cpu.InfoStat) (sockets, cores, threads int) {
	if len(infos) == 1 && infos[0].Cores > 1 {
		cores = int(infos[0].Cores)
		return 1, cores, max(runtime.NumCPU(), cores)
	}
	socketSet := map[string]bool{}
	coreSet := map[string]bool{}
	for _, info := range infos {
		threads++
		socketSet[info.PhysicalID] = true
		core := info.CoreID
		if core == "" {
			core = "cpu" + strconv.Itoa(int(info.CPU))
		}
		coreSet[info.PhysicalID+"/"+core] = true
	}
	return len(socketSet), len(coreSet), threads
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
