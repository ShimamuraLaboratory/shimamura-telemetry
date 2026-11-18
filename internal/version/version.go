// Package version はビルド時に埋め込まれるバージョン情報を保持する。
//
// 値は Makefile から -ldflags "-X ..." で上書きされる。
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version はリリースバージョン（例: v0.1.0）。
	Version = "dev"
	// Commit はビルド元の Git コミットハッシュ。
	Commit = "unknown"
	// Date はビルド日時（RFC 3339）。
	Date = "unknown"
)

// String は `shimamura-telemetry version` で表示する文字列を返す。
func String() string {
	return fmt.Sprintf("shimamura-telemetry %s (commit: %s, built: %s, %s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
