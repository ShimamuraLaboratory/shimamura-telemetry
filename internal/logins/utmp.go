// Package logins は ssh などのログイン情報（utmp / wtmp）を読み、ログイン履歴用のメッセージとして監視 Worker へ送る。
//
// メッセージの形式は shimamura-IdP の pkg/schema/src/serverMonitor/messages.ts（AgentSessionsMessage・
// AgentLoginEventsMessage）に合わせる。Unix アカウントは使用者間で共有しているため、IdP のユーザーは
// 監視 Worker が Cloudflare Access の認証ログと時刻の近さで推定する。エージェントは Unix ユーザー名だけを送る。
package logins

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"time"
)

// utmp のレコード（glibc の struct utmp）。ut_session と ut_tv の大きさがアーキテクチャで違う。
//
//	                    x86_64（384 バイト）   aarch64（400 バイト）
//	ut_type             0  (2)                 0  (2)
//	ut_pid              4  (4)                 4  (4)
//	ut_line             8  (32)  端末。例: "pts/0"
//	ut_id               40 (4)
//	ut_user             44 (32)
//	ut_host             76 (256)
//	ut_exit             332 (4)
//	ut_session          336 (4)                336 (8)
//	ut_tv.tv_sec        340 (4)                344 (8)
//	ut_tv.tv_usec       344 (4)                352 (8)
//
// x86_64 は 32 ビット環境と同じ形にするため（__WORDSIZE_TIME64_COMPAT32）時刻が 32 ビットになっている。
// 符号なしとして読むと 2106 年まで扱える。
type layout struct {
	size    int
	secOff  int
	usecOff int
	// wide は時刻が 64 ビットか。
	wide bool
}

var (
	layout384 = layout{size: 384, secOff: 340, usecOff: 344}
	layout400 = layout{size: 400, secOff: 344, usecOff: 352, wide: true}
)

// layoutFor は GOARCH に対応するレコードの形を返す。対応していないアーキテクチャでは false。
func layoutFor(goarch string) (layout, bool) {
	switch goarch {
	case "amd64":
		return layout384, true
	case "arm64":
		return layout400, true
	}
	return layout{}, false
}

// ut_type の値（utmp.h）。
const (
	typeUserProcess = 7
	typeDeadProcess = 8
)

// record は utmp / wtmp の 1 件。
type record struct {
	Type int16
	PID  int32
	Line string
	User string
	Host string
	Time time.Time
}

// parseRecord はレコードを 1 件読む。b の長さは l.size。
func (l layout) parseRecord(b []byte) record {
	var sec, usec uint64
	if l.wide {
		sec = binary.LittleEndian.Uint64(b[l.secOff:])
		usec = binary.LittleEndian.Uint64(b[l.usecOff:])
	} else {
		sec = uint64(binary.LittleEndian.Uint32(b[l.secOff:]))
		usec = uint64(binary.LittleEndian.Uint32(b[l.usecOff:]))
	}
	return record{
		Type: int16(binary.LittleEndian.Uint16(b[0:2])),
		PID:  int32(binary.LittleEndian.Uint32(b[4:8])),
		Line: cString(b[8:40]),
		User: cString(b[44:76]),
		Host: cString(b[76:332]),
		Time: time.Unix(int64(sec), int64(usec%1_000_000)*1000),
	}
}

// cString は NUL で終わる（または領域いっぱいの）文字列を読む。
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.ToValidUTF8(string(b), "")
}

// readRecords は r からレコードを読めるだけ読む。末尾の書きかけ（1 件に満たない部分）は読まない。
func (l layout) readRecords(r io.Reader, fn func(record)) error {
	buf := make([]byte, l.size)
	for {
		_, err := io.ReadFull(r, buf)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
		fn(l.parseRecord(buf))
	}
}
