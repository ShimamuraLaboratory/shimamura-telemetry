package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeProcessSource は呼ばれるたびに samples を 1 つずつ進めて返す processSource。
type fakeProcessSource struct {
	samples [][]procSample
	calls   int
	err     error
	infos   map[int32]procInfo
}

func (f *fakeProcessSource) Samples(context.Context) ([]procSample, error) {
	if f.err != nil {
		return nil, f.err
	}
	s := f.samples[min(f.calls, len(f.samples)-1)]
	f.calls++
	return s, nil
}

func (f *fakeProcessSource) Info(_ context.Context, pid int32) (procInfo, error) {
	info, ok := f.infos[pid]
	if !ok {
		return procInfo{}, errors.New("no such process")
	}
	return info, nil
}

// testClock は呼ばれるたびに step ずつ進む時計。
func testClock(step time.Duration) func() time.Time {
	t := time.Unix(1_700_000_000, 0)
	return func() time.Time {
		cur := t
		t = t.Add(step)
		return cur
	}
}

func newTestProcessCollector(src *fakeProcessSource, now func() time.Time) *processCollector {
	users := &userResolver{
		now: now,
		lookupID: func(uid string) (string, error) {
			switch uid {
			case "0":
				return "root", nil
			case "1000":
				return "alice", nil
			}
			return "", errors.New("unknown user")
		},
		getent: func(_ context.Context, uid string) (string, error) {
			if uid == "2000" {
				return "ldap-bob", nil
			}
			return "", errors.New("not found")
		},
		cache: map[uint32]userEntry{},
	}
	return &processCollector{source: src, users: users, now: now, userBudget: time.Minute}
}

func TestProcessCollectorCPUAndSelection(t *testing.T) {
	src := &fakeProcessSource{
		samples: [][]procSample{
			{
				{PID: 1, CreateTime: 100, CPUSeconds: 10, RSS: 1000},
				{PID: 2, CreateTime: 100, CPUSeconds: 10, RSS: 5000},
				// PID が使い回される
				{PID: 3, CreateTime: 100, CPUSeconds: 50, RSS: 10},
				{PID: 4, CreateTime: 100, CPUSeconds: 0, RSS: 0},
			},
			{
				// 1 秒で 2 秒分 → 200%
				{PID: 1, CreateTime: 100, CPUSeconds: 12, RSS: 1000},
				// 1 秒で 0.25 秒分 → 25%
				{PID: 2, CreateTime: 100, CPUSeconds: 10.25, RSS: 5000},
				// 別のプロセス（起動時刻が違う）→ 0%
				{PID: 3, CreateTime: 200, CPUSeconds: 60, RSS: 10},
				// CPU もメモリも使っていない → 送らない
				{PID: 4, CreateTime: 100, CPUSeconds: 0, RSS: 0},
				// GPU を使っている
				{PID: 5, CreateTime: 100, CPUSeconds: 0, RSS: 0},
			},
		},
		infos: map[int32]procInfo{
			1: {Name: "python", UID: 1000},
			2: {Name: "sshd", UID: 0},
			3: {Name: "bash", UID: 2000},
			4: {Name: "kworker", UID: 0},
			5: {Name: "train", UID: 3000},
		},
	}
	p := newTestProcessCollector(src, testClock(time.Second))
	if err := p.baseline(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := p.collect(t.Context(), map[int32]uint64{5: 4096, 999: 1})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		// GPU を使っているプロセスが先頭。UID を引けない場合は数字
		"pid=5 user=3000 name=train cpu=0 mem=0 gpu=4096",
		// CPU 使用率の上位
		"pid=1 user=alice name=python cpu=200 mem=1000 gpu=<nil>",
		"pid=2 user=root name=sshd cpu=25 mem=5000 gpu=<nil>",
		// メモリ使用量の上位（LDAP のユーザーは getent で引く）
		"pid=3 user=ldap-bob name=bash cpu=0 mem=10 gpu=<nil>",
	}
	if diff := diffProcesses(got, want); diff != "" {
		t.Error(diff)
	}
}

func TestProcessCollectorWithoutBaseline(t *testing.T) {
	src := &fakeProcessSource{
		samples: [][]procSample{{{PID: 1, CreateTime: 1, CPUSeconds: 100, RSS: 10}}},
		infos:   map[int32]procInfo{1: {Name: "a", UID: 1000}},
	}
	got, err := newTestProcessCollector(src, testClock(time.Second)).collect(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// 前回の値が無いので CPU は 0。メモリの上位としては送る
	if diff := diffProcesses(got, []string{"pid=1 user=alice name=a cpu=0 mem=10 gpu=<nil>"}); diff != "" {
		t.Error(diff)
	}
}

func TestProcessCollectorLimits(t *testing.T) {
	var prev, cur []procSample
	gpu := map[int32]uint64{}
	infos := map[int32]procInfo{}
	for i := range int32(200) {
		pid := i + 1
		prev = append(prev, procSample{PID: pid, CreateTime: 1, CPUSeconds: 0, RSS: uint64(pid)})
		cur = append(cur, procSample{PID: pid, CreateTime: 1, CPUSeconds: float64(pid), RSS: uint64(pid)})
		infos[pid] = procInfo{Name: strings.Repeat("n", MaxProcessNameLength+5), UID: 1000}
		if pid <= 90 {
			gpu[pid] = uint64(pid)
		}
	}
	src := &fakeProcessSource{samples: [][]procSample{prev, cur}, infos: infos}
	p := newTestProcessCollector(src, testClock(time.Second))
	_ = p.baseline(t.Context())
	got, err := p.collect(t.Context(), gpu)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxProcesses {
		t.Fatalf("len = %d, want %d", len(got), MaxProcesses)
	}
	// GPU の 90 件のあと、CPU の上位（PID 200 から）
	if got[0].PID != 90 || got[89].PID != 1 || got[90].PID != 200 {
		t.Errorf("order: first=%d gpu-last=%d cpu-first=%d", got[0].PID, got[89].PID, got[90].PID)
	}
	if n := len([]rune(got[0].Name)); n != MaxProcessNameLength {
		t.Errorf("name length = %d", n)
	}
	// 1 秒で pid 秒分
	if got[90].CPUPercent != 20000 {
		t.Errorf("cpu = %v", got[90].CPUPercent)
	}
}

func TestProcessCollectorSkipsExitedProcesses(t *testing.T) {
	src := &fakeProcessSource{
		samples: [][]procSample{{{PID: 1, RSS: 10}, {PID: 2, RSS: 20}}},
		// PID 2 は Info を読む前に終了した
		infos: map[int32]procInfo{1: {Name: "a", UID: 1000}},
	}
	got, err := newTestProcessCollector(src, testClock(time.Second)).collect(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PID != 1 {
		t.Errorf("got %v", got)
	}
}

func TestUserResolverCache(t *testing.T) {
	now := time.Unix(0, 0)
	lookups, getents := 0, 0
	r := &userResolver{
		now: func() time.Time { return now },
		lookupID: func(uid string) (string, error) {
			lookups++
			if uid == "1000" {
				return "alice", nil
			}
			return "", errors.New("unknown")
		},
		getent: func(context.Context, string) (string, error) {
			getents++
			return "", errors.New("not found")
		},
		cache: map[uint32]userEntry{},
	}
	for range 3 {
		if got := r.lookup(t.Context(), 1000); got != "alice" {
			t.Errorf("got %q", got)
		}
		if got := r.lookup(t.Context(), 4242); got != "4242" {
			t.Errorf("got %q", got)
		}
	}
	if lookups != 2 || getents != 1 {
		t.Errorf("lookups=%d getents=%d, want 2 and 1", lookups, getents)
	}
	// 引けなかった UID は時間が経ったら引き直す
	now = now.Add(userCacheNegativeTTL)
	r.lookup(t.Context(), 4242)
	if getents != 2 {
		t.Errorf("getents = %d, want 2", getents)
	}
}

func TestGetentPasswd(t *testing.T) {
	name, err := getentPasswd(t.Context(), "0")
	if err != nil {
		t.Skipf("getent is not available: %v", err)
	}
	if name != "root" {
		t.Errorf("name = %q, want root", name)
	}
}

func TestSystemCollectProcesses(t *testing.T) {
	src := &fakeProcessSource{
		samples: [][]procSample{
			{{PID: 7, CreateTime: 1, CPUSeconds: 0, RSS: 100}},
			{{PID: 7, CreateTime: 1, CPUSeconds: 1, RSS: 100}},
		},
		infos: map[int32]procInfo{7: {Name: "train", UID: 1000}},
	}
	s := newTestSystem(baseSource())
	s.processes = newTestProcessCollector(src, testClock(time.Second))
	s.gpu = &fakeGPUReader{gpus: []GPUMetrics{{Index: 0}}, procs: map[int32]uint64{7: 2048}}

	snap, err := s.Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 初回の Collect で基準を読んでいるので、CPU 使用率が出る
	if diff := diffProcesses(snap.Metrics.Processes, []string{"pid=7 user=alice name=train cpu=100 mem=100 gpu=2048"}); diff != "" {
		t.Error(diff)
	}

	// プロセスが読めなくても、他の値は送る
	src.err = errors.New("boom")
	snap, err = s.Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Metrics.Processes != nil {
		t.Errorf("processes = %v, want nil", snap.Metrics.Processes)
	}
}

// 実際の OS から読めることを確認する（値の正しさは見ない）。
func TestNewProcessCollectorFromOS(t *testing.T) {
	p := newProcessCollector(time.Now)
	if err := p.baseline(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := p.collect(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no processes")
	}
	for _, m := range got {
		if m.PID <= 0 || m.Name == "" || m.User == "" || m.CPUPercent < 0 {
			t.Errorf("process = %+v", m)
		}
	}
}

func diffProcesses(got []ProcessMetrics, want []string) string {
	lines := make([]string, 0, len(got))
	for _, m := range got {
		gpu := "<nil>"
		if m.GPUMemoryBytes != nil {
			gpu = fmt.Sprint(*m.GPUMemoryBytes)
		}
		lines = append(lines, fmt.Sprintf("pid=%d user=%s name=%s cpu=%v mem=%d gpu=%s", m.PID, m.User, m.Name, m.CPUPercent, m.MemoryBytes, gpu))
	}
	if strings.Join(lines, "\n") == strings.Join(want, "\n") {
		return ""
	}
	return fmt.Sprintf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
}

// LDAP などが応答しない場合でも、ユーザー名を引く時間の合計で打ち切って UID を送る。
func TestProcessCollectorUserLookupBudget(t *testing.T) {
	var samples []procSample
	infos := map[int32]procInfo{}
	for i := range int32(50) {
		pid := i + 1
		samples = append(samples, procSample{PID: pid, CreateTime: 1, RSS: uint64(pid)})
		infos[pid] = procInfo{Name: "p", UID: uint32(5000 + pid)}
	}
	src := &fakeProcessSource{samples: [][]procSample{samples}, infos: infos}
	p := newTestProcessCollector(src, time.Now)
	p.userBudget = 100 * time.Millisecond
	getents := 0
	// 応答しない getent（ctx が終わるまで待つ）
	p.users.getent = func(ctx context.Context, _ string) (string, error) {
		getents++
		<-ctx.Done()
		return "", ctx.Err()
	}

	start := time.Now()
	got, err := p.collect(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("collect took %v", d)
	}
	if len(got) != topByMemory {
		t.Fatalf("len = %d, want %d", len(got), topByMemory)
	}
	for _, m := range got {
		if m.User != fmt.Sprint(5000+m.PID) {
			t.Errorf("user = %q, want numeric uid", m.User)
		}
	}
	// 時間切れで打ち切った分は覚えず、次の収集で引き直す
	if getents != 1 {
		t.Errorf("getents = %d, want 1", getents)
	}
	if len(p.users.cache) != 0 {
		t.Errorf("cache = %v, want empty", p.users.cache)
	}
}
