# AGENTS.md

ninelives は Claude と Codex の残り使用率を RunCat Neo のカードに書く macOS 向けの Go ツール。
標準ライブラリのみ、単一パッケージ `main`、シェルスクリプト無し。ファイル構成は README の「構成」表を参照。
ここには、コードや README から導けないことだけを書く。

## コマンド

- 検証は CI と同じ順で: `gofmt -l .`（出力が空であること）→ `go vet ./...` → `go test ./...` → `GOOS=darwin GOARCH=arm64 go build ./...`
- 手動確認: `go run . -stdout` / `go run . codex -stdout`（カードを標準出力に）、`-raw`（生レスポンスと応答ヘッダ）、`install -dry-run`（plist だけ表示）
- **`install` / `uninstall` は頼まれない限り実行しない。** 開発機の launchd エージェントと `~/Library` を書き換える。
- テストはネットワークにも launchd にも触れない。`codexGet` のテストは httptest でローカルポートを listen するので、listen を禁じる sandbox では失敗する（コードの問題ではない）。

## 不変条件

- **Claude カードの JSON と plist はバイト単位で固定。** `TestClaudeCardJSONIsStable` と `TestClaudePlistGolden` が守っている。変えるときは意図的な決定として README も同じコミットで更新する。
- **失敗時に metrics ファイルを上書きしない。** RunCat に最後の成功値を残し、「◯分前」が古くなることで止まりを見せる設計。
- **429 は per-provider の state ファイルに backoff として記録し、次回以降はリクエストせず終了コード 0 で抜ける。** 60 秒未満の間隔は `-interval` が受け付けない。
- **エンドポイントは非公式。形の揺れは許容し、行の欠落で済むものはカードを落とさない。** `resetTime` は RFC3339 と Unix 秒の両方を読む。未知の `limits.kind` はそのまま行になる。`extra_usage` と reset credits は壊れても本体を巻き込まない。
- **メニューバーの値はプラン本体の窓だけ**（`-bar 5h` / `7d`）。`-extra` の未知の枠や `Review` 行がバーを乗っ取らない。`-bar min` は `rows()` の全行を見る（Claude の `7d Fable` と Codex の `Review` 行は同じ扱い）。
- **認証情報は読むだけ。トークンをログにも標準出力にも出さない。** リフレッシュは実装しない。401 は「`claude` / `codex` を一度起動」の案内にする。
- **プロバイダは完全分離。** `codex` プレフィックスで out / state / launchd ラベル / ログが別。state のキーは provider であって `-out` のファイル名ではない。プロバイダを足すときも同じ形にする。
- **エージェントは 120 秒ごとに走る。恒常的な状態は一度だけ告知する**（state の `Seen`、`ResetCreditsNote`）。毎回出るログは設計ミス。

## 実測値（変わったら日付付きで更新）

- Claude usage: 5 分あたり 5 リクエスト。6 本目で 429 と `Retry-After: 300`。成功応答にレート制限ヘッダは付かない。既定 120 秒は Claude Code 自身の `/usage` と枠を分け合うため。
- Codex: `wham/usage` と `wham/rate-limit-reset-credits` の 2 GET/回。レート制限は未計測。Resets は専用エンドポイントの `available_count` を使う。`wham/usage` 側の値は Codex アプリの表示と食い違った（2026-09 時点で 3 対 1）。
- Go 1.22 のツールチェーンは LC_UUID 無しの Mach-O を作り、現行の macOS が起動を拒む。CI とリリースは `stable` を使い、go.mod の 1.22 は言語バージョンの下限として残す。

## 作法

- コメントは英語で「なぜ」を書く。測った事実は数字ごと残す。
- README は日本語でユーザー向け。フラグ・パス・ファイル名・挙動が変わったら同じコミットで直す。
- コミットは英語の命令形の件名と、理由を書く本文。issue への言及は `For #N`。
- エラー文はユーザーが次に何をすべきかを言う（例: start `claude` once）。
- テストは実レスポンスを削ったサンプルを使い、何を防ぐテストかをコメントに書く。
- gofmt / vet / gopls の指摘ゼロを保つ。

## リリース

`v*` タグで release.yml が macOS ランナー上で darwin arm64 / amd64 を `-ldflags "-X main.version=<tag>"` 付きでビルドし、`SHA256SUMS` と一緒に GitHub Release に公開する。`go install` 経由の版はモジュールバージョンにフォールバックする（`resolveVersion`）。
