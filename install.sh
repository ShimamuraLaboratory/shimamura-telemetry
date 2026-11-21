#!/bin/sh
# shimamura-telemetry のインストールスクリプト。
#
# 計算機サーバーで root として実行し、バイナリ・設定ファイル・systemd unit を配置してエージェントを常駐させる。
# 再実行しても壊れない（冪等）。2 回目以降は、渡した項目だけ設定を書き換え、バイナリと unit を更新して再起動する。
#
# 使い方:
#   curl -fsSL https://github.com/ShimamuraLaboratory/shimamura-telemetry/releases/latest/download/install.sh | sudo sh -s -- \
#     --server-id <サーバー ID> --token <エージェントトークン> --url wss://<監視 Worker>/agent/connect
#
#   # アンインストール（設定ファイルも削除する）
#   curl -fsSL https://github.com/ShimamuraLaboratory/shimamura-telemetry/releases/latest/download/install.sh | sudo sh -s -- --uninstall
#
# オプション:
#   --server-id ID         IdP で登録した計算機サーバーの ID（初回は必須）
#   --token TOKEN          エージェントトークン（初回は必須）。プロセス一覧に出したくない場合は、
#                          代わりに環境変数 SHIMAMURA_TELEMETRY_TOKEN で渡せる
#   --url URL              監視 Worker のエンドポイント（wss:// または ws://、初回は必須）
#   --interval DURATION    収集・送信の間隔（例: 5s。省略時は既存の値、無ければ 5s）
#   --binary PATH          ダウンロードせず、手元のバイナリをインストールする
#   --version TAG          インストールするリリース（例: v0.1.0。省略時は最新）
#   --download-base URL    バイナリの取得元（<URL>/shimamura-telemetry-linux-<arch> と <URL>/checksums.txt を取得する）
#   --uninstall            サービスを止め、バイナリ・unit・設定ファイルを削除する
#   -h, --help             この説明を表示する

set -eu

REPO="ShimamuraLaboratory/shimamura-telemetry"
SERVICE="shimamura-telemetry.service"
BIN_PATH="/usr/local/bin/shimamura-telemetry"
CONFIG_DIR="/etc/shimamura-telemetry"
CONFIG_PATH="$CONFIG_DIR/config.toml"
UNIT_PATH="/etc/systemd/system/$SERVICE"
DEFAULT_INTERVAL="5s"

log() {
	printf 'shimamura-telemetry: %s\n' "$*" >&2
}

die() {
	log "error: $*"
	exit 1
}

usage() {
	cat <<'USAGE'
Usage: install.sh [options]

  --server-id ID         IdP で登録した計算機サーバーの ID（初回は必須）
  --token TOKEN          エージェントトークン（初回は必須。環境変数 SHIMAMURA_TELEMETRY_TOKEN でも渡せる）
  --url URL              監視 Worker のエンドポイント（wss:// または ws://、初回は必須）
  --interval DURATION    収集・送信の間隔（例: 5s。省略時は既存の値、無ければ 5s）
  --binary PATH          ダウンロードせず、手元のバイナリをインストールする
  --version TAG          インストールするリリース（例: v0.1.0。省略時は最新）
  --download-base URL    バイナリの取得元（<URL>/shimamura-telemetry-linux-<arch> と <URL>/checksums.txt を取得する）
  --uninstall            サービスを止め、バイナリ・unit・設定ファイルを削除する
  -h, --help             この説明を表示する
USAGE
}

server_id=""
token="${SHIMAMURA_TELEMETRY_TOKEN:-}"
url=""
interval=""
binary=""
version=""
download_base=""
uninstall=0

# 値を取るオプションの値を読む（--opt value と --opt=value の両方に対応）
need_value() {
	if [ $# -lt 2 ] || [ -z "$2" ]; then
		die "$1 requires a value"
	fi
}

while [ $# -gt 0 ]; do
	case "$1" in
	--*=*)
		opt="${1%%=*}"
		val="${1#*=}"
		shift
		set -- "$opt" "$val" "$@"
		continue
		;;
	--server-id) need_value "$@"; server_id="$2"; shift 2 ;;
	--token) need_value "$@"; token="$2"; shift 2 ;;
	--url) need_value "$@"; url="$2"; shift 2 ;;
	--interval) need_value "$@"; interval="$2"; shift 2 ;;
	--binary) need_value "$@"; binary="$2"; shift 2 ;;
	--version) need_value "$@"; version="$2"; shift 2 ;;
	--download-base) need_value "$@"; download_base="$2"; shift 2 ;;
	--uninstall) uninstall=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option: $1 (see --help)" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run as root (e.g. curl ... | sudo sh -s -- ...)"
if ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; then
	die "systemd is required"
fi

# ---- アンインストール ----

if [ "$uninstall" -eq 1 ]; then
	if systemctl list-unit-files "$SERVICE" >/dev/null 2>&1; then
		systemctl disable --now "$SERVICE" >/dev/null 2>&1 || true
	fi
	rm -f "$UNIT_PATH"
	systemctl daemon-reload
	rm -f "$BIN_PATH"
	rm -rf "$CONFIG_DIR"
	log "uninstalled (removed $BIN_PATH, $UNIT_PATH and $CONFIG_DIR)"
	exit 0
fi

# ---- 入力の検証 ----

# 設定ファイルは TOML の基本文字列（"..."）で書くため、引用符・バックスラッシュ・改行を含む値は受け付けない
check_plain() {
	case "$2" in
	*[\"\\]* | *'
'*) die "$1 must not contain quotes, backslashes or newlines" ;;
	esac
}

if [ -n "$server_id" ]; then
	check_plain --server-id "$server_id"
fi
if [ -n "$token" ]; then
	case "$token" in
	*[!A-Za-z0-9._~+/=-]*) die "--token contains unexpected characters" ;;
	esac
fi
if [ -n "$url" ]; then
	check_plain --url "$url"
	case "$url" in
	wss://?* | ws://?*) ;;
	*) die "--url must start with wss:// or ws://" ;;
	esac
fi
if [ -n "$interval" ]; then
	case "$interval" in
	*[!0-9a-z.]*) die "--interval must be a duration such as 5s" ;;
	esac
fi

case "$(uname -m)" in
x86_64 | amd64) arch="amd64" ;;
aarch64 | arm64) arch="arm64" ;;
*) die "unsupported architecture: $(uname -m) (supported: x86_64, aarch64)" ;;
esac

# 一時ファイルは置き換え先と同じディレクトリに作り、検証してから mv で置き換える（途中で失敗しても壊さない）
umask 077
install -d -m 0700 -o root -g root "$CONFIG_DIR"
install -d -m 0755 "$(dirname "$BIN_PATH")"
tmp_config="$(mktemp "$CONFIG_DIR/.config.toml.XXXXXX")"
tmp_bin="$(mktemp "$BIN_PATH.XXXXXX")"
tmp_unit="$(mktemp "$UNIT_PATH.XXXXXX")"
tmp_sums="$(mktemp)"
cleanup() {
	rm -f "$tmp_config" "$tmp_bin" "$tmp_unit" "$tmp_sums"
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

# ---- バイナリ ----
# 既存の設定ファイルを TOML として読むのにも使うため、先に用意する

download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "curl or wget is required to download the binary (or use --binary)"
	fi
}

if [ -n "$binary" ]; then
	[ -f "$binary" ] || die "binary not found: $binary"
	cp "$binary" "$tmp_bin"
else
	if [ -z "$download_base" ]; then
		if [ -n "$version" ]; then
			download_base="https://github.com/$REPO/releases/download/$version"
		else
			download_base="https://github.com/$REPO/releases/latest/download"
		fi
	fi
	asset="shimamura-telemetry-linux-$arch"
	asset_url="${download_base%/}/$asset"
	log "downloading $asset_url"
	download "$asset_url" "$tmp_bin" || die "failed to download $asset_url (use --binary or --download-base)"

	# リリースに添付した checksums.txt（sha256sum の出力）と照合する
	download "${download_base%/}/checksums.txt" "$tmp_sums" ||
		die "failed to download ${download_base%/}/checksums.txt"
	expected="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp_sums")"
	actual="$(sha256sum "$tmp_bin" | awk '{ print $1 }')"
	if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
		die "checksum mismatch for $asset (expected ${expected:-none}, got $actual)"
	fi
fi
chmod 0755 "$tmp_bin"
"$tmp_bin" version >/dev/null 2>&1 || die "the binary does not run on this machine (linux/$arch)"

# ---- 設定ファイル ----

# 省略した項目は既存の設定ファイルの値を使う。コメントや引用符の違いなど TOML として正しい書き方を
# 取りこぼさないよう、新しいバイナリ（エージェントと同じ TOML のパーサー）で読む
existing_value() {
	"$tmp_bin" check-config --config "$CONFIG_PATH" -get "$1" 2>/dev/null
}
existing_error=""
if [ -f "$CONFIG_PATH" ]; then
	if existing_error="$("$tmp_bin" check-config --config "$CONFIG_PATH" 2>&1 >/dev/null)"; then
		existing_error=""
		[ -n "$server_id" ] || server_id="$(existing_value server_id)"
		[ -n "$token" ] || token="$(existing_value agent_token)"
		[ -n "$url" ] || url="$(existing_value url)"
		[ -n "$interval" ] || interval="$(existing_value interval)"
	elif [ -z "$existing_error" ]; then
		existing_error="failed to read $CONFIG_PATH"
	fi
fi
[ -n "$interval" ] || interval="$DEFAULT_INTERVAL"

missing=""
[ -n "$server_id" ] || missing="$missing --server-id"
[ -n "$token" ] || missing="$missing --token"
[ -n "$url" ] || missing="$missing --url"
if [ -n "$missing" ]; then
	if [ -n "$existing_error" ]; then
		die "missing required options:$missing (the existing configuration could not be used: $existing_error)"
	fi
	die "missing required options:$missing (needed for the first installation)"
fi

# 既存の設定ファイルから読んだ値も、TOML の基本文字列として書けるか確かめる
check_plain server_id "$server_id"
check_plain agent_token "$token"
check_plain url "$url"
check_plain interval "$interval"

cat >"$tmp_config" <<EOF
# shimamura-telemetry の設定ファイル（install.sh が書き出した）。
# エージェントトークンを含むため、root だけが読めるようにしておくこと（0600）。

server_id = "$server_id"
agent_token = "$token"
url = "$url"
interval = "$interval"
EOF
chmod 0600 "$tmp_config"

# 新しいバイナリで新しい設定ファイルを検証する
"$tmp_bin" check-config --config "$tmp_config" >/dev/null || die "invalid configuration"

# ---- systemd unit ----
# deploy/systemd/shimamura-telemetry.service と同じ内容（テストで一致を確認している）

cat >"$tmp_unit" <<'UNIT'
# shimamura-telemetry の systemd unit。
#
# 設定ファイル /etc/shimamura-telemetry/config.toml にはエージェントトークンが含まれるため、
# root だけが読めるようにしておくこと:
#   sudo install -d -m 0700 -o root -g root /etc/shimamura-telemetry
#   sudo install -m 0600 -o root -g root config.toml /etc/shimamura-telemetry/config.toml
#
# 他ユーザーのプロセス情報や GPU（/dev/nvidia*）を読むため root で動かす。
# その代わり、書き込みやネットワーク以外の権限は下の設定で絞る。

[Unit]
Description=Shimamura telemetry agent
Documentation=https://github.com/ShimamuraLaboratory/shimamura-telemetry
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/shimamura-telemetry run --config /etc/shimamura-telemetry/config.toml
Restart=always
RestartSec=5s

User=root
Group=root
UMask=0077

# ハードニング
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
LockPersonality=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
SystemCallArchitectures=native
# GPU の情報を読む nvidia-smi は /dev/nvidia* を使うため PrivateDevices は有効にしない。

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 "$tmp_unit"

# ---- 配置と起動 ----

mv -f "$tmp_bin" "$BIN_PATH"
mv -f "$tmp_config" "$CONFIG_PATH"
mv -f "$tmp_unit" "$UNIT_PATH"
systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1
systemctl restart "$SERVICE"

# 起動直後に設定やネットワークの問題で落ちていないかを確かめる
sleep 3
if systemctl is-active --quiet "$SERVICE"; then
	log "installed $("$BIN_PATH" version) and started $SERVICE"
	log "logs: journalctl -u $SERVICE -f"
else
	log "the service is not running; recent logs:"
	journalctl -u "$SERVICE" -n 20 --no-pager >&2 || true
	exit 1
fi
