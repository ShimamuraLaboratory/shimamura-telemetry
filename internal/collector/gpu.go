package collector

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// GPU の情報は nvidia-smi の CSV 出力から読む。
//
// NVIDIA 公式の go-nvml は cgo が必須で、ビルドした環境の glibc に動的リンクされる。
// そうすると単一の静的バイナリ（CGO_ENABLED=0）で配布する方針が崩れ、古いディストリビューションで動かなくなるうえ、
// arm64 向けのクロスビルドにも C のクロスコンパイラが必要になる。nvidia-smi はドライバに同梱されているため、
// GPU のあるサーバーには必ずあり、cgo 無しで同じ値（NVML 経由）を読める。

const (
	// MaxGPUIndex は GpuMetricsSchema の index の上限。
	MaxGPUIndex = 63
	// MaxGPUNameLength は GPU 名の文字数の上限。
	MaxGPUNameLength = 255

	minTemperatureC = -50
	maxTemperatureC = 200
	maxPowerW       = 10_000

	// nvidiaSMITimeout は nvidia-smi 1 回の実行の上限。ドライバが応答しない場合に収集全体が止まらないようにする。
	nvidiaSMITimeout = 3 * time.Second

	mib = 1024 * 1024
)

// nvidiaSMIQuery は --query-gpu に渡す項目。parseNvidiaSMI はこの順に読む。
var nvidiaSMIQuery = []string{
	"index",
	"name",
	"utilization.gpu",
	"memory.total",
	"memory.used",
	"temperature.gpu",
	"power.draw",
}

// errNoNvidiaSMI は nvidia-smi が見つからない（NVIDIA のドライバが入っていない）ことを表す。
var errNoNvidiaSMI = errors.New("nvidia-smi not found")

// gpuReader は GPU の情報を読む。テストで差し替えるために分けている。
type gpuReader interface {
	ReadGPUs(ctx context.Context) ([]GPUMetrics, error)
	// ReadGPUProcesses は GPU を使っているプロセスの PID ごとの GPU メモリの使用量（バイト）を読む。
	// 複数の GPU を使うプロセスは合計する。
	ReadGPUProcesses(ctx context.Context) (map[int32]uint64, error)
}

// nvidiaSMI は nvidia-smi を実行して GPU の情報を読む gpuReader。
type nvidiaSMI struct {
	lookPath func(file string) (string, error)
	run      func(ctx context.Context, name string, args ...string) ([]byte, error)
	timeout  time.Duration
}

func newNvidiaSMI() *nvidiaSMI {
	return &nvidiaSMI{lookPath: exec.LookPath, run: runCommand, timeout: nvidiaSMITimeout}
}

// ReadGPUs は nvidia-smi で GPU ごとの情報を読む。
// nvidia-smi が無い場合は errNoNvidiaSMI を返す。
func (n *nvidiaSMI) ReadGPUs(ctx context.Context) ([]GPUMetrics, error) {
	out, err := n.query(ctx, "--query-gpu="+strings.Join(nvidiaSMIQuery, ","))
	if err != nil {
		return nil, err
	}
	return parseNvidiaSMI(out)
}

// ReadGPUProcesses は nvidia-smi で GPU を使っているプロセスを読む。
// nvidia-smi が無い場合は errNoNvidiaSMI を返す。
func (n *nvidiaSMI) ReadGPUProcesses(ctx context.Context) (map[int32]uint64, error) {
	out, err := n.query(ctx, "--query-compute-apps=pid,used_memory")
	if err != nil {
		return nil, err
	}
	return parseNvidiaSMIProcesses(out)
}

// query は nvidia-smi を CSV（ヘッダ・単位なし）で実行する。
func (n *nvidiaSMI) query(ctx context.Context, query string) ([]byte, error) {
	// ドライバが後から入る場合もあるため、毎回探す（PATH を見るだけなので軽い）
	path, err := n.lookPath("nvidia-smi")
	if err != nil {
		return nil, errNoNvidiaSMI
	}
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	return n.run(ctx, path, query, "--format=csv,noheader,nounits")
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%s: %w", name, ctxErr)
		}
		// nvidia-smi はドライバと通信できない場合などに、理由を標準出力に書いて失敗する
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return nil, fmt.Errorf("%s: %w: %s", name, err, firstLine(msg))
	}
	return out, nil
}

// parseNvidiaSMI は nvidia-smi --query-gpu の CSV（ヘッダ・単位なし）を読む。
// 取得できない項目（"[N/A]"・"[Not Supported]" など）は、温度・電力なら nil、それ以外は 0 とする。
func parseNvidiaSMI(out []byte) ([]GPUMetrics, error) {
	r := csv.NewReader(bytes.NewReader(out))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = len(nvidiaSMIQuery)

	gpus := []GPUMetrics{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse nvidia-smi output: %w", err)
		}
		index, err := strconv.Atoi(strings.TrimSpace(rec[0]))
		if err != nil {
			return nil, fmt.Errorf("parse nvidia-smi output: invalid index %q", rec[0])
		}
		if index < 0 || index > MaxGPUIndex {
			continue
		}
		if len(gpus) >= MaxGPUs {
			break
		}
		total := mibToBytes(parseNumber(rec[3]))
		gpus = append(gpus, GPUMetrics{
			Index:              index,
			Name:               truncateRunes(strings.TrimSpace(rec[1]), MaxGPUNameLength),
			UtilizationPercent: clamp(valueOr(parseNumber(rec[2]), 0), 0, 100),
			MemoryTotalBytes:   total,
			MemoryUsedBytes:    min(mibToBytes(parseNumber(rec[4])), total),
			TemperatureC:       inRange(parseNumber(rec[5]), minTemperatureC, maxTemperatureC),
			PowerW:             inRange(parseNumber(rec[6]), 0, maxPowerW),
		})
	}
	return gpus, nil
}

// parseNvidiaSMIProcesses は nvidia-smi --query-compute-apps=pid,used_memory の CSV を読む。
// 使用量が取得できない（"[N/A]"）場合は 0 とする。
func parseNvidiaSMIProcesses(out []byte) (map[int32]uint64, error) {
	r := csv.NewReader(bytes.NewReader(out))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = 2

	procs := map[int32]uint64{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse nvidia-smi output: %w", err)
		}
		pid, err := strconv.ParseInt(strings.TrimSpace(rec[0]), 10, 32)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("parse nvidia-smi output: invalid pid %q", rec[0])
		}
		procs[int32(pid)] += mibToBytes(parseNumber(rec[1]))
	}
	return procs, nil
}

// parseNumber は数値を読む。"[N/A]" などの数値でない値は nil を返す。
func parseNumber(s string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil
	}
	return &v
}

func mibToBytes(v *float64) uint64 {
	if v == nil || *v <= 0 {
		return 0
	}
	return uint64(*v * mib)
}

func valueOr(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}

func clamp(v, lo, hi float64) float64 {
	return min(max(v, lo), hi)
}

// inRange は v が [lo, hi] に収まる場合だけ返す。範囲外はセンサーの異常値として nil にする。
func inRange(v *float64, lo, hi float64) *float64 {
	if v == nil || *v < lo || *v > hi {
		return nil
	}
	return v
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
