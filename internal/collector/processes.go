package collector

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// プロセスの収集。
//
// 送るのは「GPU を使っているプロセス」「CPU 使用率の上位」「メモリ使用量の上位」を合わせたもので、
// 学習などでリソースを多く使っているのが誰の何かを、サーバー詳細で見られるようにするためのもの。
// コマンドライン引数にはパスワードやトークンが含まれうるため、送るのはコマンド名（実行ファイル名）だけにする。

const (
	// MaxProcesses は ProcessMetricsSchema の配列の上限。
	MaxProcesses = 100
	// MaxUserLength はユーザー名の文字数の上限。
	MaxUserLength = 64
	// MaxProcessNameLength はコマンド名の文字数の上限。
	MaxProcessNameLength = 255

	// topByCPU・topByMemory は CPU 使用率・メモリ使用量の上位から送る件数。
	// GPU を使っているプロセスはこれとは別に全て送る（合わせて MaxProcesses まで）。
	topByCPU    = 20
	topByMemory = 20
	// maxCPUPercent は CPUPercent の上限（ProcessMetricsSchema）。複数コアを使うプロセスは 100 を超える。
	maxCPUPercent = 100_000

	// commLength は Linux のコマンド名（/proc/<pid>/comm）の最大長。これ以上なら切り詰められている可能性がある。
	commLength = 15

	// userCacheNegativeTTL は名前を引けなかった UID を引き直すまでの時間。
	userCacheNegativeTTL = 10 * time.Minute
	// getentTimeout は getent 1 回の実行の上限。
	getentTimeout = 2 * time.Second
	// userLookupBudget は 1 回の収集でユーザー名を引くのに使える時間の合計。
	// LDAP・SSSD が応答しない場合に、送るプロセスの数だけ getent の待ちが積み重なって
	// CPU・メモリなど他の値まで送れなくなるのを防ぐ。使い切ったら残りは UID を数字のまま送る。
	userLookupBudget = 2 * time.Second
)

// procSample はプロセス 1 つ分の、順位付けに使う値。
type procSample struct {
	PID int32
	// CreateTime は起動時刻（UNIX ミリ秒）。PID が使い回された場合に別のプロセスとして扱うために使う。
	CreateTime int64
	// CPUSeconds は起動からの CPU 時間（user + system、秒）。
	CPUSeconds float64
	// RSS は物理メモリの使用量（バイト）。
	RSS uint64
}

// procInfo は送るプロセスについてだけ読む値。
type procInfo struct {
	Name string
	UID  uint32
}

// processSource は OS からプロセスの情報を読む処理。テストで差し替えるために分けている。
type processSource interface {
	// Samples は全プロセスの CPU 時間・メモリ使用量を読む。読めないプロセス（終了した・権限が無い）は含めない。
	Samples(ctx context.Context) ([]procSample, error)
	// Info はコマンド名と実行ユーザーを読む。
	Info(ctx context.Context, pid int32) (procInfo, error)
}

// gopsutilProcessSource は gopsutil で /proc から読む processSource。
type gopsutilProcessSource struct{}

func (gopsutilProcessSource) Samples(ctx context.Context) ([]procSample, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	samples := make([]procSample, 0, len(procs))
	for _, p := range procs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		times, err := p.TimesWithContext(ctx)
		if err != nil {
			continue
		}
		created, err := p.CreateTimeWithContext(ctx)
		if err != nil {
			continue
		}
		var rss uint64
		if mem, err := p.MemoryInfoWithContext(ctx); err == nil {
			rss = mem.RSS
		}
		samples = append(samples, procSample{
			PID:        p.Pid,
			CreateTime: created,
			CPUSeconds: times.User + times.System,
			RSS:        rss,
		})
	}
	return samples, nil
}

func (gopsutilProcessSource) Info(ctx context.Context, pid int32) (procInfo, error) {
	p, err := process.NewProcessWithContext(ctx, pid)
	if err != nil {
		return procInfo{}, err
	}
	name, err := p.NameWithContext(ctx)
	if err != nil {
		return procInfo{}, err
	}
	// コマンド名は 15 文字で切り詰められるため、その場合は実行ファイル名を使う
	if len(name) >= commLength {
		if exe, err := p.ExeWithContext(ctx); err == nil && exe != "" {
			name = filepath.Base(exe)
		}
	}
	uids, err := p.UidsWithContext(ctx)
	if err != nil {
		return procInfo{}, err
	}
	if len(uids) == 0 {
		return procInfo{}, errors.New("no uid")
	}
	return procInfo{Name: name, UID: uids[0]}, nil
}

// prevProc は前回読んだプロセスの CPU 時間。
type prevProc struct {
	createTime int64
	cpuSeconds float64
}

// processCollector は送るプロセスを選び、CPU 使用率を前回との差から求める。
type processCollector struct {
	source processSource
	users  *userResolver
	now    func() time.Time
	// userBudget は 1 回の収集でユーザー名を引くのに使える時間の合計（userLookupBudget）。
	userBudget time.Duration

	// prev・prevAt は前回読んだ全プロセスの CPU 時間とその時刻。
	prev   map[int32]prevProc
	prevAt time.Time
}

func newProcessCollector(now func() time.Time) *processCollector {
	return &processCollector{source: gopsutilProcessSource{}, users: newUserResolver(now), now: now, userBudget: userLookupBudget}
}

// baseline は次回の CPU 使用率の基準として、全プロセスの CPU 時間を読んでおく。
func (p *processCollector) baseline(ctx context.Context) error {
	_, _, _, err := p.sample(ctx)
	return err
}

// sample は全プロセスを読み、前回の値とその時刻を返す。読んだ値は次回の基準として保存する。
func (p *processCollector) sample(ctx context.Context) ([]procSample, map[int32]prevProc, float64, error) {
	samples, err := p.source.Samples(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	at := p.now()
	prev := p.prev
	var elapsed float64
	if !p.prevAt.IsZero() {
		elapsed = at.Sub(p.prevAt).Seconds()
	}
	p.prev = make(map[int32]prevProc, len(samples))
	for _, s := range samples {
		p.prev[s.PID] = prevProc{createTime: s.CreateTime, cpuSeconds: s.CPUSeconds}
	}
	p.prevAt = at
	return samples, prev, elapsed, nil
}

// collect は送るプロセスを選んで返す。gpuMemory は PID ごとの GPU メモリの使用量（GPU を使っていなければ含まれない）。
func (p *processCollector) collect(ctx context.Context, gpuMemory map[int32]uint64) ([]ProcessMetrics, error) {
	samples, prev, elapsed, err := p.sample(ctx)
	if err != nil {
		return nil, err
	}

	cpuPercent := make(map[int32]float64, len(samples))
	byPID := make(map[int32]procSample, len(samples))
	for _, s := range samples {
		byPID[s.PID] = s
		pp, ok := prev[s.PID]
		// 前回の値が無い（新しく起動した・PID が使い回された）プロセスは 0 とする
		if !ok || pp.createTime != s.CreateTime || elapsed <= 0 {
			continue
		}
		if d := s.CPUSeconds - pp.cpuSeconds; d > 0 {
			cpuPercent[s.PID] = min(round2(d/elapsed*100), maxCPUPercent)
		}
	}

	pids := selectProcesses(samples, cpuPercent, gpuMemory, byPID)
	userCtx, cancel := context.WithTimeout(ctx, p.userBudget)
	defer cancel()
	result := make([]ProcessMetrics, 0, len(pids))
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := p.source.Info(ctx, pid)
		if err != nil {
			// 読む間に終了した
			continue
		}
		m := ProcessMetrics{
			PID:         pid,
			User:        truncateRunes(p.users.lookup(userCtx, info.UID), MaxUserLength),
			Name:        truncateRunes(info.Name, MaxProcessNameLength),
			CPUPercent:  cpuPercent[pid],
			MemoryBytes: byPID[pid].RSS,
		}
		if mem, ok := gpuMemory[pid]; ok {
			m.GPUMemoryBytes = &mem
		}
		result = append(result, m)
	}
	return result, nil
}

// selectProcesses は送るプロセスの PID を、GPU メモリ・CPU 使用率・メモリ使用量の順に選ぶ（重複は除く）。
func selectProcesses(samples []procSample, cpuPercent map[int32]float64, gpuMemory map[int32]uint64, byPID map[int32]procSample) []int32 {
	seen := make(map[int32]bool)
	var pids []int32
	add := func(pid int32) {
		if len(pids) < MaxProcesses && !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}

	// GPU を使っているプロセスは全て（GPU メモリの多い順）。PID が見えない（既に終了した）ものは除く
	gpuPIDs := make([]int32, 0, len(gpuMemory))
	for pid := range gpuMemory {
		if _, ok := byPID[pid]; ok {
			gpuPIDs = append(gpuPIDs, pid)
		}
	}
	slices.SortFunc(gpuPIDs, func(a, b int32) int {
		return cmp.Or(cmp.Compare(gpuMemory[b], gpuMemory[a]), cmp.Compare(a, b))
	})
	for _, pid := range gpuPIDs {
		add(pid)
	}

	byCPU := slices.Clone(samples)
	slices.SortFunc(byCPU, func(a, b procSample) int {
		return cmp.Or(cmp.Compare(cpuPercent[b.PID], cpuPercent[a.PID]), cmp.Compare(a.PID, b.PID))
	})
	for _, s := range byCPU[:min(topByCPU, len(byCPU))] {
		if cpuPercent[s.PID] > 0 {
			add(s.PID)
		}
	}

	byMemory := slices.Clone(samples)
	slices.SortFunc(byMemory, func(a, b procSample) int {
		return cmp.Or(cmp.Compare(b.RSS, a.RSS), cmp.Compare(a.PID, b.PID))
	})
	for _, s := range byMemory[:min(topByMemory, len(byMemory))] {
		if s.RSS > 0 {
			add(s.PID)
		}
	}
	return pids
}

// userEntry は UID からユーザー名を引いた結果。
type userEntry struct {
	name string
	ok   bool
	at   time.Time
}

// userResolver は UID からユーザー名を引き、結果を覚えておく。
//
// CGO_ENABLED=0 でビルドした os/user は /etc/passwd しか読まないため、LDAP・SSSD などのユーザーは引けない。
// その場合は NSS を通す getent passwd で引き直す。どちらでも引けない場合は UID を数字のまま使う。
type userResolver struct {
	now      func() time.Time
	lookupID func(uid string) (string, error)
	getent   func(ctx context.Context, uid string) (string, error)
	cache    map[uint32]userEntry
}

func newUserResolver(now func() time.Time) *userResolver {
	return &userResolver{
		now: now,
		lookupID: func(uid string) (string, error) {
			u, err := user.LookupId(uid)
			if err != nil {
				return "", err
			}
			return u.Username, nil
		},
		getent: getentPasswd,
		cache:  map[uint32]userEntry{},
	}
}

// lookup は UID からユーザー名を引く。引けない場合は UID を数字のまま返す。
// ctx が終わっている（時間を使い切った）場合は getent を実行せず、結果も覚えない（次回の収集で引き直す）。
func (r *userResolver) lookup(ctx context.Context, uid uint32) string {
	id := strconv.FormatUint(uint64(uid), 10)
	if e, ok := r.cache[uid]; ok && (e.ok || r.now().Sub(e.at) < userCacheNegativeTTL) {
		if e.ok {
			return e.name
		}
		return id
	}
	name, err := r.lookupID(id)
	if err != nil || name == "" {
		if ctx.Err() != nil {
			return id
		}
		name, err = r.getent(ctx, id)
	}
	if err != nil || name == "" {
		if ctx.Err() == nil {
			r.cache[uid] = userEntry{ok: false, at: r.now()}
		}
		return id
	}
	r.cache[uid] = userEntry{name: name, ok: true, at: r.now()}
	return name
}

// getentPasswd は getent passwd <uid> の出力（name:x:uid:gid:...）からユーザー名を読む。
func getentPasswd(ctx context.Context, uid string) (string, error) {
	path, err := exec.LookPath("getent")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, getentTimeout)
	defer cancel()
	out, err := runCommand(ctx, path, "passwd", uid)
	if err != nil {
		return "", err
	}
	name, _, ok := strings.Cut(strings.TrimSpace(string(out)), ":")
	if !ok || name == "" {
		return "", fmt.Errorf("unexpected getent output: %q", out)
	}
	return name, nil
}
