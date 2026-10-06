# 登録・メール確認・OpenCode OAuthの調査と実装

## 構成例と検証状況

- Email Worker: `ocpool-email-bridge`
- API: `https://ocpool-email-bridge.<your-subdomain>.workers.dev`
- 受信ドメイン（例）: `example.com`
- D1: `ocpool-email-bridge`
- OAuth: OpenCode 1.18.34と同じdevice OAuth、CloakBrowser内承認、ネイティブDB互換保存
- SDK: `github.com/acksell/opencode-go-sdk/sdk v1.18.34`

上記のURL・ドメインは設定例であり、自分のCloudflare環境の値へ置き換える。検証環境ではWorkerをデプロイし、ローカルの実Workerランタイムでメール投入から確認URLの取得まで検証済み。公開環境ではAPI認証、受信待ち登録、取得、削除を検証済み。Catch-allのWorkerへの切り替えも確認済み。

実登録テスト `live-test-001` は **ready**。実メール受信、同じブラウザーでのメール確認、ネイティブCLIでのOAuth保存、プロセス再起動後のGo SDKによる無料モデル生成まで確認済み。`opencode/big-pickle` に対して `Reply with exactly OK.` を送り、`OK` が返った。SSE接続も成功した。検証数は1アカウントであり、数百件の連続登録の検証結果ではない。

## Email Routingの設定

最初に公開用テンプレートをローカル設定へコピーする。`email-worker/wrangler.jsonc` はGitの無視対象で、実デプロイ先に固有の設定を保存する。

```powershell
Copy-Item email-worker/wrangler.example.jsonc email-worker/wrangler.jsonc
```

コピー後、`vars.MAIL_DOMAIN` を自分の受信ドメインへ変更し、`d1_databases` の `database_name` と `database_id` を自分のCloudflare D1の値へ変更する。APIのベースURLにも、デプロイ後の `https://ocpool-email-bridge.<your-subdomain>.workers.dev` を設定する。

Cloudflareの `example.com` → Email Routing → Routing rulesで、Catch-allを以下に設定する。

- Action: **Send to a Worker**
- Destination: **ocpool-email-bridge**
- Enabled: **ON**

Workerは、APIから事前登録した宛先へのメールを受け取る。受信待ちがない宛先はSMTPで拒否する。テストメールを単に任意のアドレスへ送るだけでは保管されない。

## 実行方法

PowerShellでプロジェクトのルートから実行する。

```powershell
npm.cmd ci
go build -o bin/opencode-probe.exe ./cmd/opencode-probe
node scripts/check-routing.mjs
node scripts/onboard.mjs --id first-account
```

デフォルトではジョブ専用のランダムな `ocp-...@example.com` を使う。特定のアドレスを指定する場合は `--email address@example.com` を追加する。新規ジョブは一つのアカウントを扱い、繰り返し実行によるアカウントの自動大量作成は行わない。

同じIDで再実行すると進捗を読み込む。

```powershell
node scripts/onboard.mjs --id first-account --status
node scripts/onboard.mjs --id first-account --mail-timeout 180
```

登録・メール認証だけを調べる場合は `--signup-only` を付ける。ブラウザー処理は **CloakBrowser** を使う。Nodeラッパーは0.5.12、確認済みバイナリは146.0.7680.177.5。既定はheadlessで、利用者が画面を操作する場合は `--headed` で専用プロファイルを開ける。

画面を表示する場合は固定viewportを設定せず、実際のウィンドウサイズに追従する。高さ550pxの作業領域でも確認画面が収まるよう、ジョブ専用プロファイルの表示倍率を67%にしている。倍率は `OCPOOL_BROWSER_ZOOM`、バイナリのバージョンは `OCPOOL_BROWSER_VERSION` で変更できる。

画像認証が表示された場合は、以下で専用画面を開き、確認完了まで最大15分待つ。同じジョブのアドレス・パスワード・ブラウザープロファイルを引き継ぐ。

```powershell
node scripts/onboard.mjs --id live-test-001 --headed --human-wait
```

完了済みのジョブを再実行すると結果を表示し、新たな登録やログインは行わない。OAuth保存済みのジョブは、ブラウザーを起動せず疎通確認から再開する。完了後にもう一度疎通確認する場合は `--recheck` を使う。登録送信中にウィンドウを閉じた場合は、登録未完了を確認してから `--retry-signup` で再開する。

Catch-allを確認するスクリプトは、先にWranglerでOAuthの更新を行ってからCloudflare APIを参照する。時間が経過したWranglerのアクセストークンを、そのまま使うことで起きていた403を修正した。

`opencode console login` はOS既定ブラウザーを無条件で開き、CLIに `--no-browser` がありません。この副作用を避けるため、`scripts/oauth-headless.mjs` が同じclient ID `opencode-cli` でdevice OAuthを実行します。ジョブの既存CloakBrowserだけで承認し、URLのorigin・device code・ブラウザーのメール・返されたトークンの本人情報を照合します。`authorization_pending` / `slow_down` に従って待機します。

非表示の `opencode db` でネイティブDBの初期化とmigrationを行い、スキーマを確認した上で、分離プロファイルのaccount / account_stateへトランザクションで保存します。別人のプロファイルには保存しません。その後のトークン更新と推論はOpenCodeに任せます。既存アカウントの再認証と無料モデル生成まで実機確認済みです。

## フロー

1. Workerへ対象アドレスの受信待ちを登録する。
2. 専用の永続ブラウザープロファイルでConsoleのsignupを開き、メールアドレスと生成したパスワードを送る。
3. WorkerのAPIから届いたメールの確認URLを取得する。
4. 登録と同じブラウザー内で確認URLを開き、ログイン状態を確認する。
5. Consoleのdevice OAuthを `opencode-cli` として開始する。
6. 発行されたURLを同じCloakBrowserで開き、本人のメールアドレスとdevice codeを確認して認可する。
7. トークンの本人情報を確認し、分離したネイティブDBへ保存した後、同じ保存先で `opencode serve` を起動する。
8. Go SDKでhealth・モデル一覧・SSE接続・無料モデルへの短い生成を確認する。

メールのURLをWorker側から自動で開くことはしない。ブラウザーのセッションを維持するのはローカルの登録ジョブ。

## 実機調査で分かった差分

- メール登録画面は `https://opencode.ai/console/signup`。メールとパスワードを入力する。ページにはhCaptchaが組み込まれている。
- メール確認URLのページは `/console/verify-email`。確認後にConsoleのセッション状態を確認できる。
- OpenCode自身のConsole OAuthは `console login`。`auth login` はプロバイダーの資格情報登録であり、今回の処理とは別。
- ConsoleアカウントはOpenCodeのaccountテーブルに保存され、active account/orgが選択される。旧auth.jsonやV2 credentialテーブルと混同しない。
- `/api/integration/opencode` は今回の分離した実機ランタイムでは `data: null`。この経路でConsole OAuthを始める設計にはしない。
- Acksell SDKのREADMEには `@sdk/v1.18.34` とあるが、この指定は実際のGoコマンドで `disallowed version string` になった。導入に成功した指定は `go get github.com/acksell/opencode-go-sdk/sdk@v1.18.34`。
- 最初のEdge実行では画像認証が表示され、固定viewportが実画面より大きく操作領域が見切れた。CloakBrowserと実ウィンドウ追従・縮小表示へ切り替えた後、同じジョブで登録が成功した。対話確認が出る場合の待機・再開は引き続き用意する。
- 初回生成は `OpenCode's free tier can only be used from within OpenCode` で失敗した。同じ認証状態の公式CLIでも再現し、検証用に指定した `permission: {"*":"deny"}` を `ask` にすると、同じバージョン・モデルでSDK生成が成功した。ツール操作は引き続き確認が必要で、自動許可にはしていない。
- 今回は新規アカウントのPersonalワークスペースが自動で作られ、OAuth認可時に追加のログイン・workspace作成操作は不要だった。

## Worker API

`GET /health` 以外は `Authorization: Bearer <API_TOKEN>` が必要。

| API | 用途 |
| --- | --- |
| `POST /v1/inboxes` | `{"email":"...@example.com","ttl_seconds":900}` で受信待ちを作成 |
| `GET /v1/inboxes/{id}/messages` | 宛先に届いたメールのURL・6桁コード候補を取得 |
| `DELETE /v1/inboxes/{id}` | 受信待ちと確認データを削除 |

同時に有効な受信待ちは同じ宛先に一つ。ジョブごとの別アドレスで区別する。受信データは原文を保持せず、sender・subject・URL・コード候補のみ。TTLは1分〜1時間、メール本文上限256 KiB、保管件数は宛先ごとに20件。期限切れは即座に取得不可となり、10分間隔の清掃でテーブルから削除される。D1のバックアップ保持期間は別管理。

## ローカル状態

- `.state/email-bridge.json`: APIの接続先と利用トークン
- `.state/worker-secrets.json`: Wranglerへの秘密情報投入用
- `.state/onboarding/<id>/job.json`: メール・パスワード・進捗
- `.state/onboarding/<id>/browser-cloak/`: 登録中のCloakBrowser状態（旧Edgeの `browser/` は引き継がず保持）
- `.state/onboarding/<id>/opencode/`: OpenCodeの分離したdata/config/cache/state
- `*.png` / 状態JSON / CLIログ: `.state` 内のジョブディレクトリ

`.state` はgit対象外とし、Windowsでは現在の利用者だけのACLを設定している。トークンとパスワードは標準出力へ表示しない。ジョブデータを共有する場合は秘密情報を含む原本をそのまま渡さない。

## 検証コマンド

```powershell
npm.cmd test
npx.cmd wrangler d1 migrations apply ocpool-email-bridge --local --config email-worker/wrangler.jsonc
npm.cmd run email:dev
# 別のターミナルから
node scripts/smoke-email.mjs --local
node scripts/smoke-email.mjs
node scripts/probe-sdk.mjs
```

`smoke-email --local` はCloudflareのローカルメール投入APIを利用する。公開APIにはメールを偽装投入するエンドポイントを設けていない。公開環境のsmokeは受信待ちの作成・取得・削除のみで、実メール受信の成功を意味しない。

## 参照

- [Cloudflare Email Worker](https://developers.cloudflare.com/email-service/api/route-emails/email-handler/)
- [ローカルのメール受信テスト](https://developers.cloudflare.com/email-service/local-development/routing/)
- [OpenCode Console CLI](https://github.com/anomalyco/opencode/blob/v1.18.34/packages/opencode/src/cli/cmd/account.ts)
- [Consoleアカウントの保存](https://github.com/anomalyco/opencode/blob/v1.18.34/packages/opencode/src/account/repo.ts)
- [Acksell SDK](https://github.com/Acksell/opencode-go-sdk)
- [CloakBrowser JavaScript API](https://github.com/CloakHQ/CloakBrowser/blob/main/js/README.md)
- [ツール拒否設定で同じ無料モデルエラーが発生する報告](https://github.com/anomalyco/opencode/issues/51241)
