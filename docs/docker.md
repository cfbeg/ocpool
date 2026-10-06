# Dockerでの起動

OCPool本体、OpenCode CLI、Node.js、CloakBrowser、WireGuard出口プールをDockerだけで起動できます。ホスト側にGo・Node.js・OpenCode CLIをインストールする必要はありません。通常は`ocpool`（管理画面/API）と`surfshark-pool`（複数WireGuard接続）の2サービスをComposeで起動します。

ブラウザーはCloakBrowserのheadlessモードでコンテナー内に保持されます。hCaptchaなど人の操作が必要なジョブは管理画面の「登録ジョブ → 詳細・ブラウザー」から表示・操作します。GUIを持たないサーバーでは、`--headed`ではなく管理画面の画面転送を使ってください。

## 初回セットアップ

Linux Docker EngineまたはDocker DesktopのLinux engineを用意し、リポジトリのルートで実行します。WindowsのPowerShellから実行する場合も、Docker側はLinuxコンテナーで動作させます。

```powershell
New-Item -ItemType Directory -Force .state/server, .state/onboarding, .state/surfshark/pool-state | Out-Null
```

`wg.conf`はリポジトリ直下へ置きます。WireGuardの秘密鍵をGitへ追加したり、Composeのコマンドラインへ直接書いたりしないでください。カタログ作成だけはNodeコンテナーで行い、接続先の公開鍵・DNS・ICMP情報を`.state/surfshark/servers.json`へ保存します。OCPoolの通常運用にはホストのGo・Node.js・OpenCode CLIは不要です。

```powershell
node scripts/surfshark-catalog.mjs --wg-config wg.conf --probe-count all
node -e "const{randomBytes}=require('node:crypto');const fs=require('node:fs');fs.mkdirSync('.state/surfshark/pool-state',{recursive:true});if(!fs.existsSync('.state/surfshark/relay-pool-token.txt'))fs.writeFileSync('.state/surfshark/relay-pool-token.txt',randomBytes(32).toString('hex')+'\\n',{mode:0o600})"
```

Node.jsをホストへ入れない場合の準備（Linux Docker Engine）:

```sh
docker run --rm -v "$PWD:/work" -w /work node:24-bookworm \
  sh -lc 'node scripts/surfshark-catalog.mjs --wg-config wg.conf --probe-count all && node -e "const{randomBytes}=require(\"node:crypto\");const fs=require(\"node:fs\");fs.mkdirSync(\".state/surfshark/pool-state\",{recursive:true});if(!fs.existsSync(\".state/surfshark/relay-pool-token.txt\"))fs.writeFileSync(\".state/surfshark/relay-pool-token.txt\",randomBytes(32).toString(\"hex\")+\"\\n\",{mode:0o600})"'

`surfshark-pool`はComposeが直接起動し、全カタログを読み取り専用でマウントして既定4接続を作ります。`setup-surfshark.mjs`は旧構成の移行や候補の実測を行う高度な管理用スクリプトとして残しています。
```

Dockerソケットを渡すコンテナーはDockerを操作できるため、準備が終わったら削除してください。Windows Docker Desktopでは、PowerShellの`${PWD}`を使い、Linux engineのソケット共有を有効にします。

上記のセットアップスクリプトは、カタログを読み取り専用でマウントした`surfshark-pool`を作成し、`wg0`〜`wg3`を維持します。`--connections`は同時接続数で、2〜32の範囲です。カタログの全候補を同時起動するのではなく、故障・IP制限・重複IPの接続を全候補からランダムに交換します。

## 起動と確認

カタログとrelay tokenを準備した後は、Composeファイルから次の操作だけで起動できます。

```powershell
docker compose up -d --build
docker compose ps
docker compose logs --tail=100 ocpool surfshark-pool
```

管理画面は`http://127.0.0.1:10000`、OpenAI互換APIは`http://127.0.0.1:10000/v1`です。初回のログインURLと管理キーは、ホストの`.state/server/access-url.txt`および`admin-key.txt`に保存されます。Compose内部では`ocpool`が`surfshark-pool:18071`へ接続し、ホストへ公開するWireGuard管理ポートは`127.0.0.1:18100`です。

出口プールの管理ポートはホストのループバックだけへ公開します。管理画面の「出口 / IP」では、各slotの接続先、測定済み公開IP、ハンドシェイク、処理中ストリーム、休止期限を確認できます。コンテナー内のHTTP/DNSソケットはslotのWireGuardインターフェイスへ固定され、トンネル停止時に通常回線へ漏れません。管理APIを直接確認する場合は、Composeが生成したrelay tokenを`X-Relay-Auth`ヘッダーへ渡して`/health`を呼び出します。tokenをコマンド履歴やログへ出さないでください。

## 永続化と秘密情報

Composeの`ocpool`には、少なくとも次をホストディレクトリへバインドします。

| パス | 内容 |
| --- | --- |
| `.state/server` | SQLite/WAL、管理キー、APIキー、レスポンス、設定 |
| `.state/onboarding` | アカウント登録情報、CloakBrowserプロファイル、OAuth状態 |
| `.state/surfshark` | relay token、公開カタログ、IP/model休止期限 |
| `wg.conf` | WireGuardの秘密鍵を含む入力ファイル（read-only） |

`.state`は700、秘密ファイルは600にします。バックアップはコンテナー停止後に行い、`pool.db`と`onboarding`を別々にコピーしてください。実行中のブラウザープロファイルやSQLite WALだけを部分コピーしないでください。

既存のWindows実行時データを移す場合は、まずWindows側のOCPoolを停止し、バックアップを作ってから同じ`.state`をComposeへマウントします。ブラウザーのロックファイルや一時ファイルはコピーせず、OAuthトークンを含むためバックアップを一般公開しないでください。

## 更新・停止

```powershell
docker compose up -d --build
docker compose stop
docker compose down
```

WireGuardプールを更新する場合は、管理画面またはセットアップスクリプトのドレインを使います。実行中のSSEが終了するまでslotを保持し、その後に別の候補へ交換します。`docker compose down -v`は永続ボリュームを削除するため、通常の更新には使いません。

## Email Worker

CloudflareのEmail RoutingはCloudflareのエッジで受信するため、実メールの受信経路をDocker内だけで再現することはできません。Worker本体のビルドとデプロイは、ローカルNode.jsを使わずWranglerコンテナーから実行できます。

初回は `email-worker/wrangler.example.jsonc` をGitの無視対象の `email-worker/wrangler.jsonc` へコピーし、`vars.MAIL_DOMAIN` とD1の `database_name`・`database_id` を自分の値へ変更します。以下の `example.com` とWorker名も、自分のデプロイ先に合わせてください。

```powershell
Copy-Item email-worker/wrangler.example.jsonc email-worker/wrangler.jsonc
docker run --rm -it -v "${PWD}:/work" -w /work node:24-bookworm sh -lc "npm ci && npx wrangler deploy --config email-worker/wrangler.jsonc"
```

Cloudflare DashboardのEmail Routingで、`example.com`のcatch-allを`Send to a Worker → ocpool-email-bridge`へ向けます。WorkerからOCPoolへ到達させるには、`email-worker`が参照するOCPoolのHTTPS公開URLとAPIトークンを設定します。OCPoolをループバックだけで待ち受ける構成ではCloudflareから到達できません。ローカル確認はWranglerのD1/localモードで行い、実メール経路とは分けてください。

## トラブルシューティング

- `surfshark-pool`が`NET_ADMIN`またはWireGuard作成で失敗する場合は、Docker DesktopのLinux engineを選び、Composeサービスに`cap_add: [NET_ADMIN]`があることを確認します。
- `ready_count`が接続数未満の場合、同じ公開IPの候補やハンドシェイク未完了の候補を使用せず、ログの候補交換を確認します。ICMP応答だけでは利用可能と判定しません。
- 管理画面が空の場合は、`ocpool`の`/app`に実行時ルートと、ホスト`.state`（`server`・`onboarding`を含む）がマウントされているか確認します。
- 既存のホスト版サーバーが同じSQLiteを開いたままDockerを起動しないでください。manager lockで拒否されるか、データ破損の原因になります。
