package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func ptr(v float64) *float64 { return &v }

func TestParseNvidiaSMI(t *testing.T) {
	out := strings.Join([]string{
		"0, NVIDIA GeForce RTX 4090, 35, 24564, 1024, 45, 120.50",
		// 取得できない項目
		"1, NVIDIA A100-SXM4-80GB, [N/A], 81920, [N/A], [N/A], [Not Supported]",
		// 名前にカンマを含む場合は引用符で囲まれる。範囲外の値
		`2, "Tesla, Custom", 150, 100, 200, 300, -5`,
	}, "\n") + "\n"

	got, err := parseNvidiaSMI([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []GPUMetrics{
		{
			Index: 0, Name: "NVIDIA GeForce RTX 4090", UtilizationPercent: 35,
			MemoryTotalBytes: 24564 * mib, MemoryUsedBytes: 1024 * mib,
			TemperatureC: ptr(45), PowerW: ptr(120.5),
		},
		{
			Index: 1, Name: "NVIDIA A100-SXM4-80GB", UtilizationPercent: 0,
			MemoryTotalBytes: 81920 * mib, MemoryUsedBytes: 0,
		},
		{
			// 使用率は 100 に丸め、使用量は総量を超えない。温度・電力の異常値は nil
			Index: 2, Name: "Tesla, Custom", UtilizationPercent: 100,
			MemoryTotalBytes: 100 * mib, MemoryUsedBytes: 100 * mib,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNvidiaSMI() =\n%s\nwant\n%s", dumpGPUs(got), dumpGPUs(want))
	}
}

func TestParseNvidiaSMIEmpty(t *testing.T) {
	got, err := parseNvidiaSMI(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %#v, want empty non-nil slice", got)
	}
}

func TestParseNvidiaSMILimits(t *testing.T) {
	var b strings.Builder
	for i := range MaxGPUs + 4 {
		fmt.Fprintf(&b, "%d, gpu, 1, 1, 1, 1, 1\n", i)
	}
	// index の上限を超えるものは飛ばす
	fmt.Fprintf(&b, "%d, gpu, 1, 1, 1, 1, 1\n", MaxGPUIndex+1)
	got, err := parseNvidiaSMI([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxGPUs {
		t.Errorf("len = %d, want %d", len(got), MaxGPUs)
	}

	long := strings.Repeat("あ", MaxGPUNameLength+10)
	got, err = parseNvidiaSMI([]byte("0, " + long + ", 1, 1, 1, 1, 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got[0].Name)); n != MaxGPUNameLength {
		t.Errorf("name length = %d, want %d", n, MaxGPUNameLength)
	}
}

func TestParseNvidiaSMIInvalid(t *testing.T) {
	for _, out := range []string{
		"0, gpu, 1, 1\n",
		"x, gpu, 1, 1, 1, 1, 1\n",
	} {
		if _, err := parseNvidiaSMI([]byte(out)); err == nil {
			t.Errorf("parseNvidiaSMI(%q) succeeded, want error", out)
		}
	}
}

func TestNvidiaSMIReadGPUs(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		n := &nvidiaSMI{
			lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
			run: func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("run should not be called")
				return nil, nil
			},
			timeout: time.Second,
		}
		if _, err := n.ReadGPUs(t.Context()); !errors.Is(err, errNoNvidiaSMI) {
			t.Fatalf("err = %v, want errNoNvidiaSMI", err)
		}
	})
	t.Run("runs with query and timeout", func(t *testing.T) {
		var gotName string
		var gotArgs []string
		var hasDeadline bool
		n := &nvidiaSMI{
			lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
			run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				gotName, gotArgs = name, args
				_, hasDeadline = ctx.Deadline()
				return []byte("0, gpu, 10, 100, 50, 40, 70\n"), nil
			},
			timeout: time.Second,
		}
		gpus, err := n.ReadGPUs(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(gpus) != 1 || gpus[0].UtilizationPercent != 10 {
			t.Errorf("gpus = %s", dumpGPUs(gpus))
		}
		wantArgs := []string{
			"--query-gpu=index,name,utilization.gpu,memory.total,memory.used,temperature.gpu,power.draw",
			"--format=csv,noheader,nounits",
		}
		if gotName != "/usr/bin/nvidia-smi" || !reflect.DeepEqual(gotArgs, wantArgs) {
			t.Errorf("ran %s %v", gotName, gotArgs)
		}
		if !hasDeadline {
			t.Error("context has no deadline")
		}
	})
	t.Run("command error", func(t *testing.T) {
		n := &nvidiaSMI{
			lookPath: func(string) (string, error) { return "nvidia-smi", nil },
			run: func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("driver not loaded")
			},
			timeout: time.Second,
		}
		if _, err := n.ReadGPUs(t.Context()); err == nil || errors.Is(err, errNoNvidiaSMI) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRunCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	t.Run("error message from stdout", func(t *testing.T) {
		_, err := runCommand(t.Context(), "sh", "-c", "echo 'NVIDIA-SMI has failed'; echo second; exit 9")
		if err == nil || !strings.Contains(err.Error(), "NVIDIA-SMI has failed") || strings.Contains(err.Error(), "second") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := runCommand(ctx, "sleep", "10")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want deadline exceeded", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("took %v", d)
		}
	})
}

type fakeGPUReader struct {
	gpus []GPUMetrics
	err  error
}

func (f *fakeGPUReader) ReadGPUs(context.Context) ([]GPUMetrics, error) {
	return f.gpus, f.err
}

func TestSystemCollectGPUs(t *testing.T) {
	var logs bytes.Buffer
	gpu := &fakeGPUReader{}
	s := newTestSystem(baseSource())
	s.gpu = gpu
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))

	collect := func() []GPUMetrics {
		t.Helper()
		snap, err := s.Collect(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if snap.Metrics.GPUs == nil {
			t.Fatal("gpus must not be nil")
		}
		return snap.Metrics.GPUs
	}

	// nvidia-smi が無い: 空で返し、ログは 1 回だけ
	gpu.err = errNoNvidiaSMI
	collect()
	collect()
	if n := strings.Count(logs.String(), "nvidia-smi not found"); n != 1 {
		t.Errorf("not found logged %d times:\n%s", n, logs.String())
	}

	// 読めるようになった
	gpu.err = nil
	gpu.gpus = []GPUMetrics{{Index: 0, Name: "gpu"}}
	if got := collect(); len(got) != 1 {
		t.Errorf("gpus = %s", dumpGPUs(got))
	}

	// エラー: 空で返し、ログは 1 回だけ
	gpu.err = errors.New("driver not loaded")
	gpu.gpus = nil
	if got := collect(); len(got) != 0 {
		t.Errorf("gpus = %s", dumpGPUs(got))
	}
	collect()
	if n := strings.Count(logs.String(), "failed to collect gpus"); n != 1 {
		t.Errorf("error logged %d times:\n%s", n, logs.String())
	}
}

func TestSystemCollectGPUsCanceled(t *testing.T) {
	s := newTestSystem(baseSource())
	ctx, cancel := context.WithCancel(t.Context())
	s.gpu = readerFunc(func(context.Context) ([]GPUMetrics, error) {
		cancel()
		return nil, context.Canceled
	})
	if _, err := s.Collect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

type readerFunc func(context.Context) ([]GPUMetrics, error)

func (f readerFunc) ReadGPUs(ctx context.Context) ([]GPUMetrics, error) { return f(ctx) }

func dumpGPUs(gpus []GPUMetrics) string {
	var b strings.Builder
	for _, g := range gpus {
		fmt.Fprintf(&b, "  %+v temp=%v power=%v\n", g, deref(g.TemperatureC), deref(g.PowerW))
	}
	return b.String()
}

func deref(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
