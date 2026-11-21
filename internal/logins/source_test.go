package logins

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// layouts は x86_64（384 バイト）と aarch64（400 バイト）の両方で同じテストをするために使う。
var layouts = map[string]layout{"amd64": layout384, "arm64": layout400}

func forLayouts(t *testing.T, fn func(t *testing.T, l layout)) {
	t.Helper()
	for name, l := range layouts {
		t.Run(name, func(t *testing.T) { fn(t, l) })
	}
}

// rec は utmp / wtmp のレコードを作る（glibc の struct utmp の配置）。
func (l layout) rec(typ int16, pid int32, line, user, host string, at time.Time) []byte {
	b := make([]byte, l.size)
	binary.LittleEndian.PutUint16(b[0:2], uint16(typ))
	binary.LittleEndian.PutUint32(b[4:8], uint32(pid))
	copy(b[8:40], line)
	copy(b[44:76], user)
	copy(b[76:332], host)
	if l.wide {
		binary.LittleEndian.PutUint64(b[l.secOff:], uint64(at.Unix()))
		binary.LittleEndian.PutUint64(b[l.usecOff:], uint64(at.Nanosecond()/1000))
	} else {
		binary.LittleEndian.PutUint32(b[l.secOff:], uint32(at.Unix()))
		binary.LittleEndian.PutUint32(b[l.usecOff:], uint32(at.Nanosecond()/1000))
	}
	return b
}

func newSource(l layout, utmp, wtmp string) *Source {
	return &Source{utmpPath: utmp, wtmpPath: wtmp, layout: l, alive: func(int32) bool { return true }}
}

func TestLayoutFor(t *testing.T) {
	if l, ok := layoutFor("amd64"); !ok || l.size != 384 {
		t.Errorf("amd64 = %+v, %v", l, ok)
	}
	if l, ok := layoutFor("arm64"); !ok || l.size != 400 {
		t.Errorf("arm64 = %+v, %v", l, ok)
	}
	if _, ok := layoutFor("riscv64"); ok {
		t.Error("riscv64 should not be supported")
	}
}

// aarch64 の実機（Debian bookworm）の utmp の先頭レコード（BOOT_TIME）。時刻は 64 ビットで 344 バイト目から。
func TestParseRecordArm64Sample(t *testing.T) {
	b := make([]byte, 400)
	b[0] = 2
	copy(b[8:], "~")
	copy(b[40:], "~~")
	copy(b[44:], "reboot")
	copy(b[76:], "6.10.14-linuxkit")
	copy(b[344:], []byte{0x99, 0x93, 0x01, 0x6b, 0, 0, 0, 0})
	copy(b[352:], []byte{0xb2, 0xde, 0x0e, 0, 0, 0, 0, 0})
	r := layout400.parseRecord(b)
	if r.Type != 2 || r.User != "reboot" || r.Host != "6.10.14-linuxkit" {
		t.Errorf("record = %+v", r)
	}
	if got, want := r.Time, time.Unix(1795265433, 974514000); !got.Equal(want) {
		t.Errorf("time = %v, want %v", got, want)
	}
}

func writeFile(t *testing.T, path string, records ...[]byte) {
	t.Helper()
	var b []byte
	for _, r := range records {
		b = append(b, r...)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

var t0 = time.Date(2026, 11, 21, 10, 0, 0, 123_456_000, time.UTC)

func TestParseRecord(t *testing.T) {
	forLayouts(t, testParseRecord)
}

func testParseRecord(t *testing.T, l layout) {
	r := l.parseRecord(l.rec(typeUserProcess, 4242, "pts/0", "lab", "192.0.2.1", t0))
	if r.Type != typeUserProcess || r.PID != 4242 || r.Line != "pts/0" || r.User != "lab" || r.Host != "192.0.2.1" {
		t.Errorf("record = %+v", r)
	}
	if got := r.Time.UnixMilli(); got != t0.UnixMilli() {
		t.Errorf("time = %d, want %d", got, t0.UnixMilli())
	}

	// 領域いっぱいで NUL が無い文字列
	long := strings.Repeat("u", 32)
	if got := l.parseRecord(l.rec(typeUserProcess, 1, "", long, "", t0)).User; got != long {
		t.Errorf("user = %q", got)
	}
}

func TestSessions(t *testing.T) {
	forLayouts(t, testSessions)
}

func testSessions(t *testing.T, l layout) {
	dir := t.TempDir()
	utmp := filepath.Join(dir, "utmp")
	writeFile(t, utmp,
		l.rec(2, 0, "~", "reboot", "6.8.0", t0.Add(-time.Hour)),              // BOOT_TIME
		l.rec(6, 10, "tty1", "LOGIN", "", t0.Add(-time.Hour)),                // LOGIN_PROCESS
		l.rec(typeUserProcess, 100, "pts/1", "lab", "", t0.Add(time.Minute)), // host 無し
		l.rec(typeUserProcess, 101, "pts/0", "lab", "localhost", t0),
		l.rec(typeUserProcess, 102, "pts/2", "ghost", "x", t0), // プロセスが無い（古いエントリ）
		l.rec(typeDeadProcess, 103, "pts/3", "", "", t0),       // ログアウト済み
		l.rec(typeUserProcess, 104, "pts/4", "", "", t0),       // ユーザー名が無い
	)
	s := newSource(l, utmp, filepath.Join(dir, "wtmp"))
	s.alive = func(pid int32) bool { return pid != 102 }

	got, err := s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	want := `[{"user":"lab","tty":"pts/0","host":"localhost","loginAt":` + itoa(t0.UnixMilli()) +
		`},{"user":"lab","tty":"pts/1","host":null,"loginAt":` + itoa(t0.Add(time.Minute).UnixMilli()) + `}]`
	if string(b) != want {
		t.Errorf("sessions =\n%s\nwant\n%s", b, want)
	}

	// utmp が無い
	if _, err := newSource(l, filepath.Join(dir, "none"), "").Sessions(); err == nil {
		t.Error("expected error for missing utmp")
	}
}

func TestSessionsEmptyIsNotNull(t *testing.T) {
	dir := t.TempDir()
	utmp := filepath.Join(dir, "utmp")
	writeFile(t, utmp)
	got, err := newSource(layout384, utmp, "").Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(got); string(b) != "[]" {
		t.Errorf("sessions = %s, want []", b)
	}
}

func TestEventsIncremental(t *testing.T) {
	forLayouts(t, testEventsIncremental)
}

func testEventsIncremental(t *testing.T, l layout) {
	dir := t.TempDir()
	wtmp := filepath.Join(dir, "wtmp")
	writeFile(t, wtmp,
		l.rec(typeUserProcess, 1, "pts/0", "lab", "localhost", t0.Add(-48*time.Hour)), // 24 時間より前
		l.rec(typeUserProcess, 2, "pts/0", "lab", "localhost", t0),
		l.rec(2, 0, "~", "reboot", "", t0),                              // BOOT_TIME は使わない
		l.rec(typeDeadProcess, 2, "pts/0", "", "", t0.Add(time.Minute)), // ユーザー名の無いログアウト
		l.rec(typeDeadProcess, 3, "", "", "", t0.Add(time.Minute)),      // 端末の無いログアウトは送らない
	)
	s := newSource(l, "", wtmp)

	backlog, err := s.Backlog(t0.Add(-24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(backlog)
	want := `[{"type":"login","user":"lab","tty":"pts/0","host":"localhost","at":` + itoa(t0.UnixMilli()) +
		`},{"type":"logout","user":null,"tty":"pts/0","at":` + itoa(t0.Add(time.Minute).UnixMilli()) + `}]`
	if string(b) != want {
		t.Errorf("backlog =\n%s\nwant\n%s", b, want)
	}

	// 続きが無ければ何も返さない
	if got, err := s.NewEvents(); err != nil || len(got) != 0 {
		t.Fatalf("NewEvents = %v, %v", got, err)
	}

	// 書きかけ（1 件に満たない）の部分は、書き終わってから読む
	next := l.rec(typeUserProcess, 5, "pts/1", "alice", "", t0.Add(2*time.Minute))
	appendFile(t, wtmp, next[:100])
	if got, _ := s.NewEvents(); len(got) != 0 {
		t.Fatalf("read a partial record: %v", got)
	}
	appendFile(t, wtmp, next[100:])
	got, err := s.NewEvents()
	if err != nil || len(got) != 1 || got[0].User != "alice" || got[0].Host != "" {
		t.Fatalf("NewEvents = %+v, %v", got, err)
	}
	if b, _ := json.Marshal(got[0]); !strings.Contains(string(b), `"host":null`) {
		t.Errorf("login without host = %s, want host null", b)
	}
}

func TestEventsRotation(t *testing.T) {
	forLayouts(t, testEventsRotation)
}

func testEventsRotation(t *testing.T, l layout) {
	dir := t.TempDir()
	wtmp := filepath.Join(dir, "wtmp")
	writeFile(t, wtmp, l.rec(typeUserProcess, 1, "pts/0", "old", "", t0), l.rec(typeUserProcess, 2, "pts/1", "old", "", t0))
	s := newSource(l, "", wtmp)
	if _, err := s.Backlog(t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// ローテーション（別のファイルに入れ替わる）
	if err := os.Rename(wtmp, wtmp+".1"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, wtmp, l.rec(typeUserProcess, 3, "pts/2", "new", "", t0.Add(time.Hour)))
	got, err := s.NewEvents()
	if err != nil || len(got) != 1 || got[0].User != "new" {
		t.Fatalf("after rotation: %+v, %v", got, err)
	}

	// 同じファイルが切り詰められた
	writeFile(t, wtmp)
	appendFile(t, wtmp, l.rec(typeUserProcess, 4, "pts/3", "truncated", "", t0.Add(2*time.Hour)))
	got, err = s.NewEvents()
	if err != nil || len(got) != 1 || got[0].User != "truncated" {
		t.Fatalf("after truncation: %+v, %v", got, err)
	}

	// wtmp が無い
	if got, err := newSource(l, "", filepath.Join(dir, "none")).NewEvents(); err != nil || got != nil {
		t.Errorf("missing wtmp: %v, %v", got, err)
	}
}

func TestTruncateLongFields(t *testing.T) {
	r := record{Type: typeUserProcess, User: "u", Line: "pts/0", Host: strings.Repeat("h", 300), Time: t0}
	e, ok := toEvent(r)
	if !ok || len(e.Host) != maxHostLength {
		t.Errorf("host length = %d", len(e.Host))
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
