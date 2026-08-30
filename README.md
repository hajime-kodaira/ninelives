# ninelives

Claude の**残り使用率**を [RunCat Neo](https://kyome.io/runcat/) のメニューバーに出す macOS 向けの小さな Go ツールです。猫の九生（nine lives）＝残機、という名前です。

```
🩺 57%      ← メニューバー
┌──────────────────────────┐
│ Claude                   │
│ 5h        57% left · 1h36m  ████████░░░░░ │
│ 7d        93% left · 3d18h  ████████████░ │
│ 7d Fable  89% left · 3d18h  ████████████░ │
└──────────────────────────┘
```

`-lives` を付けると `57%` の代わりに `6/9` 表示になります。

## 何をしているか

Claude Code の `/usage` が内部で叩いているエンドポイント `GET https://api.anthropic.com/api/oauth/usage` から、5時間 / 週間 / モデル別週間の消費率を取り、RunCat Neo のカスタムメトリクス形式の JSON に書き出します。RunCat 側はそのファイルをファイルシステムイベントで監視していて、書き換わった瞬間にカードを更新します（ポーリングもネットワークアクセスもなし）。

認証には Claude Code が既に保存している OAuth トークンをそのまま使います（macOS Keychain の `Claude Code-credentials`、なければ `~/.claude/.credentials.json`）。**使用量の上限はアカウント単位で共有されている**ので、この数字は Claude Desktop の Settings → Usage や claude.ai で見えるものと同じです。API キーも `.env` も要りません。

依存は Go の標準ライブラリのみです。

## 必要なもの

- macOS
- [RunCat Neo](https://kyome.io/runcat/)
- Go 1.22 以降（ビルド時のみ）
- Claude Code でログイン済みであること（`claude` を一度起動すればトークンが保存されます）

## インストール

```bash
git clone https://github.com/hajime-kodaira/ninelives.git
cd ninelives
./install.sh
```

`install.sh` は次のことをします。

1. `~/bin/ninelives` にビルド
2. 一度実行して `~/.config/runcat-neo-metrics/claude.json` を生成
3. 5分間隔で実行する LaunchAgent（`io.local.ninelives`）を登録

`6/9` 表示にしたい場合は `LIVES=1 ./install.sh`、インストール先を変えたい場合は `BIN=/usr/local/bin/ninelives ./install.sh` のように環境変数で指定できます。

最後に RunCat Neo 側で登録します。

> **設定 → メトリクス → カスタムメトリクス → +**
> `~/.config` は隠しディレクトリでファイル選択ダイアログに出てこないので、**Cmd+Shift+G** でパスを直接貼り付けてください。

アンインストールは `./uninstall.sh`（RunCat 側のカード削除だけ手動です）。

## 手動で使う

```bash
go build -o ninelives .

./ninelives -stdout          # 書き込まず標準出力に出す
./ninelives -raw             # API の生レスポンスを見る
./ninelives -out ~/somewhere/claude.json
```

| フラグ | 既定値 | 説明 |
| --- | --- | --- |
| `-out` | `~/.config/runcat-neo-metrics/claude.json` | 出力先 |
| `-lives` | false | `57%` ではなく `6/9` で表示する |
| `-bar` | `5h` | メニューバーに出す窓。`5h` / `7d` / `min`（最も残りが少ないもの） |
| `-title` | `Claude` | カードのタイトル |
| `-symbol` | `staroflife` | カードの SF Symbol 名 |
| `-stdout` | false | ファイルに書かず標準出力へ |
| `-raw` | false | API の生レスポンスを表示して終了 |
| `-timeout` | `15s` | HTTP タイムアウト |

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

## 注意点

**エンドポイントは非公式です。** 公式ドキュメントに記載がなく、ベータヘッダが `oauth-2025-04-20` と日付付きになっている＝一度は変わっている証拠なので、将来予告なく壊れる可能性があります。壊れたら `-raw` でレスポンスを見てください。レスポンスの形が変わっても動くよう、新しい `limits` 配列と古い `five_hour` / `seven_day` の両方を読めるようにしてあります。

**5分間隔より短くしないでください。** このエンドポイントは 429 がかなり厳しく、頻繁に叩くと恒常的にレート制限される報告があります。

**失敗しても JSON を上書きしません。** RunCat 側は最後に成功した値を保持したまま「◯分前」だけが古くなるので、止まっていることが見て分かります。エラーは `~/Library/Logs/ninelives.log` に残ります。

**401 が続くとき**は Claude Code のトークンが失効しています。更新は Claude Code 自身がやるので、`claude` を一度起動すれば直ります。

**Keychain について。** LaunchAgent から `security find-generic-password` でトークンを読むため、ログインキーチェーンがアンロックされている必要があります（通常のログインセッションなら自動的にアンロックされています）。初回にアクセス許可のダイアログが出た場合は許可してください。

## ライセンス

MIT。詳細は [LICENSE](LICENSE) を参照してください。
