# shimamura-telemetry

計算機サーバーのリソース使用状況・稼働状況を Shimamura IdP へ送るテレメトリ収集エージェント

## 概要

各計算機サーバーで常駐し、CPU・メモリ・SSD・GPU（nvidia-smi）・プロセスの情報を一定間隔（既定 5 秒）で収集して、
Shimamura IdP の監視 Worker（Cloudflare Durable Objects）へ外向きの WebSocket で送ります。
サーバーは Cloudflare Zero Trust の内側にあるため、エージェントから外へ接続する形をとります。

- 仕様: shimamura-IdP の Issue #527（エージェント本体）、#526（メッセージ形式と監視 Worker）
- メッセージ形式は shimamura-IdP の `pkg/schema/src/serverMonitor/messages.ts`（プロトコルバージョン 1）に従います

> [!NOTE]
> 現在は開発中です。CPU・メモリ・SSD・プロセス（gopsutil）・GPU（nvidia-smi）の収集と、監視 Worker への送信（WebSocket）は実装済みです。
> インストールスクリプト（Issue #6）とリリース（Issue #7）は未実装です。

## 必要なもの

- Go 1.25 以上
- [golangci-lint](https://golangci-lint.run/) v2（`make lint` を使う場合）

> [!TIP]
> golangci-lint は、それ自体をビルドした Go より新しい Go の標準ライブラリを読めません。
> 手元の Go が新しくて `make lint` が typecheck エラーになる場合は、
> `GOTOOLCHAIN=go1.25.4 make lint` のように Go のバージョンを合わせて実行してください。

## ビルド方法

```bash
# 手元の OS/アーキテクチャ向けにビルド（bin/shimamura-telemetry）
make build

# linux/amd64 と linux/arm64 向けにビルド（bin/shimamura-telemetry-linux-{amd64,arm64}）
make build-linux

# テスト
make test

# Lint
make lint
```

ビルドした CLI の使い方:

```bash
# バージョンを表示
./bin/shimamura-telemetry version

# リソース情報を 1 回収集して JSON で表示（設定ファイル不要。送信メッセージの measuredAt・metrics と同じ形）
./bin/shimamura-telemetry collect

# エージェントを起動（既定の設定ファイルは /etc/shimamura-telemetry/config.toml）
./bin/shimamura-telemetry run --config /etc/shimamura-telemetry/config.toml
```

## 設定

設定ファイルは TOML で、既定の場所は `/etc/shimamura-telemetry/config.toml` です。
例は [`deploy/config.example.toml`](deploy/config.example.toml) を参照してください。

| キー | 必須 | 説明 |
| --- | --- | --- |
| `server_id` | ○ | IdP 側で登録した計算機サーバーの ID |
| `agent_token` | ○ | 監視 Worker へ接続するためのエージェントトークン |
| `url` | ○ | 監視 Worker の WebSocket エンドポイント（`wss://` または `ws://`） |
| `interval` |  | 収集・送信の間隔。`"5s"` のように書く（既定 `5s`、1 秒以上） |

設定ファイルにはエージェントトークンが含まれるため、root だけが読めるようにしてください。
所有者以外が読める場合、起動時に警告を出します。

```bash
sudo install -d -m 0700 -o root -g root /etc/shimamura-telemetry
sudo install -m 0600 -o root -g root deploy/config.example.toml /etc/shimamura-telemetry/config.toml
```

## systemd での運用

unit のひな形は [`deploy/systemd/shimamura-telemetry.service`](deploy/systemd/shimamura-telemetry.service) にあります。
他ユーザーのプロセス情報や GPU を読むため root で動かし、その代わりにハードニングの設定で権限を絞っています。

### プロセスについて

リソースを多く使っているのが誰の何かをサーバー詳細で見られるよう、次のプロセスを合わせて最大 100 件送ります。

- GPU を使っているプロセス（全て。`nvidia-smi --query-compute-apps` で読む）
- CPU 使用率の上位 20 件（前回の収集からの CPU 時間の差から求める。複数コアを使うと 100% を超える）
- メモリ使用量（RSS）の上位 20 件

送るのは PID・ユーザー名・コマンド名・CPU 使用率・メモリ使用量・GPU メモリ使用量だけです。
コマンドライン引数にはパスワードやトークンが含まれうるため送りません。
コマンド名は Linux では 15 文字で切り詰められるため、その場合は実行ファイル名を使います。
ユーザー名は `/etc/passwd` で引けない場合（LDAP・SSSD など）は `getent passwd` で引き、それでも引けなければ UID を送ります。

### 監視 Worker との接続

- 設定ファイルの `url`（例: `wss://idp-monitor.example.com/agent/connect`）へ、`Authorization: Bearer <agent_token>` を付けて WebSocket で接続します
- 切断・接続の失敗後は指数バックオフ（1 秒から倍々で最大 1 分、ジッター付き）で再接続します。1 分以上つながっていた接続が切れた場合は 1 秒から数え直します
- 再接続を待っている間に収集したメトリクスは送らずに捨てます（リアルタイム表示用のため、後からまとめて送りません）
- 認証に失敗した（401）・トークンが再発行された場合は、設定ファイルを直すまで成功しないため 5 分ごとに再接続を試みます
- 同じトークンで別のエージェントが接続して切られた場合は、互いに切り合わないよう 1 分待ってから再接続します

### GPU について

GPU の情報は NVIDIA のドライバに同梱されている `nvidia-smi` を実行して読みます（NVML の値と同じです）。
公式の go-nvml は cgo が必須で、単一の静的バイナリとして配布できなくなるため使っていません。

- `nvidia-smi` が PATH に無いサーバーでは GPU を空として送り、CPU・メモリ・SSD は送り続けます
- ドライバが応答しない場合も同様です（1 回の実行は 3 秒で打ち切ります）。ログは状態が変わったときだけ出します

```bash
sudo install -m 0755 bin/shimamura-telemetry-linux-amd64 /usr/local/bin/shimamura-telemetry
sudo install -m 0644 deploy/systemd/shimamura-telemetry.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now shimamura-telemetry.service
```

## ディレクトリ構成

```
.
├── cmd/
│   └── shimamura-telemetry/   # CLI のエントリポイント（run / version サブコマンド）
├── internal/
│   ├── agent/                 # 収集と送信を一定間隔で繰り返すループ
│   ├── collector/             # リソース情報の収集処理（CPU・メモリ・SSD は gopsutil）
│   ├── config/                # 設定ファイルの読み込みと検証
│   ├── transport/             # 監視 Worker への送信処理（WebSocket・再接続）
│   └── version/               # ビルド時に埋め込むバージョン情報
├── deploy/
│   ├── config.example.toml    # 設定ファイルの例
│   └── systemd/
│       └── shimamura-telemetry.service
├── .github/workflows/ci.yml   # CI（golangci-lint、go test、linux/amd64・arm64 のビルド）
├── .golangci.yml
└── Makefile
```
