# ninelives

Claude の**残り使用率**を [RunCat Neo](https://kyome.io/runcat/) のメニューバーに出す macOS 向けの小さな Go ツールです。猫の九生（nine lives）＝残機、という名前です。

```
🩺 57%      ← メニューバー
┌───────────────────────────────┐
│ Claude                        │
│ 5h        57% left · 1h36m  ████████░░░░░ │
│ 7d        93% left · 3d18h  ████████████░ │
│ 7d Fable  89% left · 3d18h  ████████████░ │
└───────────────────────────────┘
```

`-lives` を付けると `57%` の代わりに `6/9` 表示になります。

## 何をしているか

Claude Code の `/usage` が内部で叩いているエンドポイント `GET https://api.anthropic.com/api/oauth/usage` から、5時間 / 週間 / モデル別週間の消費率を取り、RunCat Neo のカスタムメトリクス形式の JSON に書き出します。RunCat 側はそのファイルをファイルシステムイベントで監視していて、書き換わった瞬間にカードを更新します（ポーリングもネットワークアクセスもなし）。

認証には Claude Code が既に保存している OAuth トークンをそのまま使います（macOS Keychain の `Claude Code-credentials`、なければ `~/.claude/.credentials.json`）。**使用量の上限はアカウント単位で共有されている**ので、この数字は Claude Desktop の Settings → Usage や claude.ai で見えるものと同じです。API キーも `.env` も要りません。

依存は Go の標準ライブラリのみで、シェルスクリプトもありません。定期実行の launchd エージェント登録まで含めて `ninelives install` の一発で完結します。

## 必要なもの

- macOS
- [RunCat Neo](https://kyome.io/runcat/)
- Go 1.22 以降（ビルド時のみ）
- Claude Code でログイン済みであること（`claude` を一度起動すればトークンが保存されます）

## インストール

```bash
git clone git@github.com:hajime-kodaira/ninelives.git
cd ninelives
go run . install
```

`go run` のバイナリは一時ディレクトリに作られて消えてしまうので、`install` はそれを検知して自動的に `~/bin/ninelives` へコピーし、そのパスを launchd に登録します。`6/9` 表示にしたいなら `go run . install -lives` です。

自分でビルド先を決めたい場合はこちら。

```bash
go build -o ~/bin/ninelives .
~/bin/ninelives install
```

`install` がやることは3つです。

1. 一度フェッチして `~/.config/runcat-neo-metrics/claude.json` を生成する（トークンが無ければ**ここで失敗する**ので、壊れた設定が登録されることはありません）
2. `~/Library/LaunchAgents/io.local.ninelives.plist` を書く
3. `launchctl bootstrap` で読み込む（再実行時は先に `bootout` するので何度でも上書きできます）

登録前に中身を見たいときは `ninelives install -dry-run` で plist だけ標準出力に出ます。

最後に RunCat Neo 側で登録します。

> **設定 → メトリクス → カスタムメトリクス → +**
> `~/.config` は隠しディレクトリでファイル選択ダイアログに出てこないので、**Cmd+Shift+G** でパスを直接貼り付けてください。

動いているかは `ninelives status` で確認できます。

```
agent    loaded
plist    /Users/you/Library/LaunchAgents/io.local.ninelives.plist
log      /Users/you/Library/Logs/ninelives.log
metrics  /Users/you/.config/runcat-neo-metrics/claude.json
         updated 2m14s ago · bar 57%
         5h         57% left · 1h36m
         7d         93% left · 3d18h
         7d Fable   89% left · 3d18h
```

アンインストールは `ninelives uninstall`（RunCat 側のカード削除だけ手動です）。

## サブコマンド

```
ninelives [flags]              1回だけ取得して metrics ファイルを書く
ninelives install [flags]      5分間隔で更新する launchd エージェントを登録する
ninelives uninstall            エージェントを解除して書いたものを消す
ninelives status               登録状況と最後に書いたカードを表示する
ninelives version
```

| フラグ | 既定値 | 説明 |
| --- | --- | --- |
| `-out` | `~/.config/runcat-neo-metrics/claude.json` | 出力先 |
| `-lives` | false | `57%` ではなく `6/9` で表示する |
| `-bar` | `5h` | メニューバーに出す窓。`5h` / `7d` / `min`（最も残りが少ないもの） |
| `-title` | `Claude` | カードのタイトル |
| `-symbol` | `staroflife` | カードの SF Symbol 名 |
| `-timeout` | `15s` | HTTP タイムアウト |
| `-raw` | false | API の生レスポンスを表示して終了（`run` のみ） |
| `-stdout` | false | ファイルに書かず標準出力へ（`run` のみ） |
| `-bin` | | バイナリをここにコピーしてそれを登録する（`install` のみ） |
| `-interval` | `300` | 更新間隔（秒）。300 が下限（`install` のみ） |
| `-dry-run` | false | 登録せず plist を表示して終了（`install` のみ） |
| `-keep-metrics` | false | metrics ファイルは消さない（`uninstall` のみ） |

`install` に付けた表示系フラグはそのまま plist の引数に埋め込まれます。既定値のままのフラグは書かれないので、生成される plist は読みやすいままです。

出力される JSON はこの形です。`normalizedValue` がある行にだけバーが描かれます。

```json
{
  "title": "Claude",
  "symbol": "staroflife",
  "metricsBarValue": "57%",
  "metrics": [
    { "title": "5h", "formattedValue": "57% left · 1h36m", "normalizedValue": 0.57 },
    { "title": "7d", "formattedValue": "93% left · 3d18h", "normalizedValue": 0.93 }
  ],
  "lastUpdatedDate": "2026-08-30T16:33:14Z"
}
```

## 構成

| ファイル | 役割 |
| --- | --- |
| `main.go` | サブコマンドの振り分けとフラグ |
| `usage.go` | 認証トークンの取得と usage エンドポイント |
| `card.go` | RunCat Neo のカード生成と整形 |
| `install.go` | launchd エージェントの登録・解除・状態表示 |

## 注意点

**エンドポイントは非公式です。** 公式ドキュメントに記載がなく、ベータヘッダが `oauth-2025-04-20` と日付付きになっている＝一度は変わっている証拠なので、将来予告なく壊れる可能性があります。壊れたら `-raw` でレスポンスを見てください。レスポンスの形が変わっても動くよう、新しい `limits` 配列と古い `five_hour` / `seven_day` の両方を読めるようにしてあります。

**5分間隔より短くしないでください。** このエンドポイントは 429 がかなり厳しく、頻繁に叩くと恒常的にレート制限される報告があります。`-interval` は 300 未満を受け付けません。

**失敗しても JSON を上書きしません。** RunCat 側は最後に成功した値を保持したまま「◯分前」だけが古くなるので、止まっていることが見て分かります。エラーは `~/Library/Logs/ninelives.log` に残ります。

**401 が続くとき**は Claude Code のトークンが失効しています。更新は Claude Code 自身がやるので、`claude` を一度起動すれば直ります。

**Keychain について。** launchd エージェントから `security find-generic-password` でトークンを読むため、ログインキーチェーンがアンロックされている必要があります（通常のログインセッションなら自動的にアンロックされています）。初回にアクセス許可のダイアログが出た場合は許可してください。

## ライセンス

MIT。詳細は [LICENSE](LICENSE) を参照してください。
