// Package deploy は配布用のファイル（systemd unit・設定ファイルの例・install.sh）の整合性をテストする。
package deploy

import (
	"os"
	"strings"
	"testing"
)

// install.sh に埋め込んだ unit が deploy/systemd/shimamura-telemetry.service と一致することを確認する。
// install.sh は curl | sh で単体で実行されるため、unit をファイルとして読めず埋め込んでいる。
func TestInstallScriptEmbedsSystemdUnit(t *testing.T) {
	script, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile("systemd/shimamura-telemetry.service")
	if err != nil {
		t.Fatal(err)
	}

	const begin = "cat >\"$tmp_unit\" <<'UNIT'\n"
	const end = "\nUNIT\n"
	_, rest, ok := strings.Cut(string(script), begin)
	if !ok {
		t.Fatal("unit heredoc not found in install.sh")
	}
	embedded, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatal("end of unit heredoc not found in install.sh")
	}
	if embedded+"\n" != string(unit) {
		t.Errorf("the unit embedded in install.sh differs from deploy/systemd/shimamura-telemetry.service; copy the file into install.sh")
	}
}
