package logins

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"
)

// メッセージ形式（LoginSessionSchema・LoginEventSchema）の上限。
const (
	maxUserLength = 64
	maxTTYLength  = 64
	maxHostLength = 255
	// MaxSessions は sessions メッセージの一覧の上限。
	MaxSessions = 256
	// MaxEventsPerMessage は login_events メッセージ 1 件あたりのイベント数の上限。
	MaxEventsPerMessage = 300
)

// 既定のファイルの場所（Linux）。
const (
	DefaultUtmpPath = "/var/run/utmp"
	DefaultWtmpPath = "/var/log/wtmp"
)

// Session は現在ログイン中のセッション（LoginSessionSchema）。
type Session struct {
	User string `json:"user"`
	TTY  string `json:"tty"`
	// Host は接続元。無ければ nil（null）。
	Host *string `json:"host"`
	// LoginAt はログイン時刻（UNIX ミリ秒）。
	LoginAt int64 `json:"loginAt"`
}

// Event はログイン・ログアウトのイベント（LoginEventSchema）。
type Event struct {
	// Logout はログアウト（DEAD_PROCESS）なら true。
	Logout bool
	// User はユーザー名。ログアウトでは空のことがある（null で送る）。
	User string
	TTY  string
	// Host は接続元。ログインのみ。無ければ空（null で送る）。
	Host string
	// At は時刻（UNIX ミリ秒）。
	At int64
}

// MarshalJSON はログインとログアウトで形を変えて書く（ログインは host が必須、ログアウトは host を持たない）。
func (e Event) MarshalJSON() ([]byte, error) {
	if e.Logout {
		return json.Marshal(struct {
			Type string  `json:"type"`
			User *string `json:"user"`
			TTY  string  `json:"tty"`
			At   int64   `json:"at"`
		}{"logout", optional(e.User), e.TTY, e.At})
	}
	return json.Marshal(struct {
		Type string  `json:"type"`
		User string  `json:"user"`
		TTY  string  `json:"tty"`
		Host *string `json:"host"`
		At   int64   `json:"at"`
	}{"login", e.User, e.TTY, optional(e.Host), e.At})
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Source は utmp（現在のログイン）と wtmp（ログイン・ログアウトの記録）を読む。
type Source struct {
	utmpPath string
	wtmpPath string
	layout   layout
	// alive は PID のプロセスが動いているかを返す。utmp に残った古いエントリを除くために使う。
	alive func(pid int32) bool

	// wtmp をどこまで読んだか。ファイルが入れ替わった（ローテーションされた）・切り詰められたら先頭から読み直す。
	// 切り詰めた後に同じ大きさまで書かれた場合も気付けるよう、先頭のレコードも覚えておく。
	wtmpInfo   os.FileInfo
	wtmpHead   []byte
	wtmpOffset int64
}

// NewSource は Source を作る。utmp のレコードの形が分からないアーキテクチャ（amd64・arm64 以外）ではエラーを返す。
func NewSource(utmpPath, wtmpPath string) (*Source, error) {
	l, ok := layoutFor(runtime.GOARCH)
	if !ok {
		return nil, fmt.Errorf("utmp is not supported on %s", runtime.GOARCH)
	}
	return &Source{utmpPath: utmpPath, wtmpPath: wtmpPath, layout: l, alive: processAlive}, nil
}

func processAlive(pid int32) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat("/proc/" + strconv.Itoa(int(pid)))
	return err == nil
}

// Sessions は現在ログイン中のセッション（utmp の USER_PROCESS）を読む。
// 異常終了などで utmp に残ったままのエントリ（プロセスが無いもの）は除く。ログイン時刻・端末の順に並べる。
func (s *Source) Sessions() ([]Session, error) {
	f, err := os.Open(s.utmpPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	sessions := []Session{}
	err = s.layout.readRecords(bufio.NewReader(f), func(r record) {
		if r.Type != typeUserProcess || r.User == "" || r.Time.Unix() <= 0 || !s.alive(r.PID) {
			return
		}
		sessions = append(sessions, Session{
			User:    truncate(r.User, maxUserLength),
			TTY:     truncate(r.Line, maxTTYLength),
			Host:    optional(truncate(r.Host, maxHostLength)),
			LoginAt: r.Time.UnixMilli(),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.utmpPath, err)
	}
	slices.SortFunc(sessions, func(a, b Session) int {
		return cmp.Or(cmp.Compare(a.LoginAt, b.LoginAt), cmp.Compare(a.TTY, b.TTY), cmp.Compare(a.User, b.User))
	})
	if len(sessions) > MaxSessions {
		sessions = sessions[len(sessions)-MaxSessions:]
	}
	return sessions, nil
}

// Backlog は wtmp を先頭から読み、since 以降のイベントを返す。以降の NewEvents はこの続きから読む。
// 接続直後に直近のイベントを送り直すために使う。
func (s *Source) Backlog(since time.Time) ([]Event, error) {
	s.wtmpInfo, s.wtmpHead, s.wtmpOffset = nil, nil, 0
	events, err := s.NewEvents()
	if err != nil {
		return nil, err
	}
	cutoff := since.UnixMilli()
	return slices.DeleteFunc(events, func(e Event) bool { return e.At < cutoff }), nil
}

// NewEvents は前回読んだところから wtmp を読み、新しいイベントを返す。
// ファイルが入れ替わった・短くなった（ローテーションされた）場合は先頭から読む。wtmp が無ければ何も返さない。
func (s *Source) NewEvents() ([]Event, error) {
	f, err := os.Open(s.wtmpPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := int64(s.layout.size)
	head := make([]byte, size)
	if n, _ := io.ReadFull(f, head); int64(n) < size {
		head = nil
	}
	if s.wtmpInfo == nil || !os.SameFile(s.wtmpInfo, info) || info.Size() < s.wtmpOffset || !bytes.Equal(head, s.wtmpHead) {
		s.wtmpOffset = 0
	}
	s.wtmpInfo, s.wtmpHead = info, head
	if info.Size()-s.wtmpOffset < size {
		return nil, nil
	}
	if _, err := f.Seek(s.wtmpOffset, io.SeekStart); err != nil {
		return nil, err
	}

	var events []Event
	read := int64(0)
	err = s.layout.readRecords(bufio.NewReader(io.LimitReader(f, info.Size()-s.wtmpOffset)), func(r record) {
		read += size
		if e, ok := toEvent(r); ok {
			events = append(events, e)
		}
	})
	s.wtmpOffset += read
	if err != nil {
		return events, fmt.Errorf("read %s: %w", s.wtmpPath, err)
	}
	return events, nil
}

// toEvent は wtmp のレコードをイベントにする。ログイン（USER_PROCESS）とログアウト（DEAD_PROCESS）以外は使わない。
func toEvent(r record) (Event, bool) {
	at := r.Time.UnixMilli()
	if at <= 0 {
		return Event{}, false
	}
	switch r.Type {
	case typeUserProcess:
		if r.User == "" {
			return Event{}, false
		}
		return Event{
			User: truncate(r.User, maxUserLength),
			TTY:  truncate(r.Line, maxTTYLength),
			Host: truncate(r.Host, maxHostLength),
			At:   at,
		}, true
	case typeDeadProcess:
		// 端末が分からないログアウトは、監視 Worker が対応するログインを見つけられないため送らない
		if r.Line == "" {
			return Event{}, false
		}
		return Event{
			Logout: true,
			User:   truncate(r.User, maxUserLength),
			TTY:    truncate(r.Line, maxTTYLength),
			At:     at,
		}, true
	}
	return Event{}, false
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
