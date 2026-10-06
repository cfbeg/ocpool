# OCPool

OpenCodeのアカウント登録・OAuth認証・アカウントプールを管理するGo製サーバーです。管理画面はGoバイナリに内蔵されています。

## 起動

Windowsでの動作を確認しています。Go、Node.js、OpenCode CLIが必要です。

### Dockerで全サービスを起動する

ホストへGo・Node.js・OpenCode CLIを入れずに運用する場合は、管理サーバー、OpenCode、
CloakBrowser、WireGuard出口プールをLinuxコンテナーで起動します。Composeの構成、
永続化ディレクトリ、秘密情報、既存データの移行、Cloudflare Email Workerの扱いは
[Docker運用ガイド](docs/docker.md)を参照してください。

```powershell
npm.cmd ci
go build -o bin/opencode-probe.exe ./cmd/opencode-probe
go build -o bin/ocpool.exe ./cmd/ocpool
.\bin\ocpool.exe --listen 127.0.0.1:10000
```

バックグラウンドで起動する場合:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/start-server.ps1
```

初回起動時に `.state/server/access-url.txt` を作成します。その中のURLをブラウザーで開くとログインできます。管理キー自体は `.state/server/admin-key.txt` に保存されます。ログイン後はURLのキーを除去し、HttpOnly Cookieを使用します。

標準の待受は `http://127.0.0.1:10000`、OpenAI互換APIの接続先は `http://127.0.0.1:10000/v1` です。外部ネットワークへ公開するためのデプロイ・TLS設定は含めていません。

## Web画面でできること

- アカウントの登録、一括登録キュー、検索、タグ、ページング、有効化・停止
- 既存の `.state/onboarding/` プロファイルの取り込み
- 登録ジョブの進捗、ログ、停止、再開、登録送信の再試行
- ジョブ専用CloakBrowserの画面表示、クリック、キー入力、スクロール
- アカウントごとの再認証、ブラウザーを開く、疎通確認
- 実行中のOpenCodeプロセスの確認・停止
- APIキーの作成・無効化
- プレイグラウンドでのストリーミング生成
- 同時稼働数、キュー、タイムアウト、Email Workerの接続設定
- 出口/IPの管理、WARPの疎通確認・再接続、IP別の休止状態・履歴

登録処理は1件ずつ実行し、OpenCodeの実行枠を共有します。ブラウザーの確認操作が必要な場合は「登録ジョブ → 詳細・ブラウザー」から操作します。管理画面側のウィンドウサイズに合わせて画面を縮小表示するため、サーバーのデスクトップを直接操作する必要はありません。

「一覧から削除」は無効化・アーカイブで、OAuthプロファイル自体を消去しません。サーバー停止時に実行中だった登録ジョブは中断状態で保存し、勝手に新規登録を再送しません。

再開したジョブが成功すると、元の「確認が必要」ジョブも「再開後に完了」へ更新されます。OAuthは既存のCloakBrowser内で承認し、OSの既定ブラウザーを開きません。

## API

管理画面の「APIキー」でクライアント用キーを発行します。`Authorization: Bearer <key>` を付けて利用します。

| エンドポイント | 対応内容 |
| --- | --- |
| `GET /v1/models` | 利用可能アカウントの保存済みモデル一覧 |
| `POST /v1/chat/completions` | テキスト、推論、function tools、通常応答、SSE |
| `POST /v1/responses` | テキスト、推論、function tools、SSE、previous_response_id、store |
| `GET /v1/responses/{id}` | 同じAPIキーで保存したレスポンスの取得 |

```json
{
  "model": "opencode/big-pickle",
  "messages": [{ "role": "user", "content": "こんにちは" }],
  "stream": true,
  "stream_options": { "include_usage": true }
}
```

Responsesの保存期限は30分です。会話の継続は同じアカウントに固定し、上流セッションをforkして元のレスポンスへの分岐を保持します。期限切れセッションの削除は稼働中のOpenCodeへまとめ、休止中のアカウントは清掃だけのために起動しません。

`temperature`、`top_p`、`max_tokens` / `max_completion_tokens`、Responsesの `max_output_tokens` は、セッションごとの設定ファイルとOpenCodeの `chat.params` フックを通して適用します。トークン上限はモデル上限までに制限します。Chatの `stop` はチャンク境界をまたいでも除去し、`n` は1〜8件を順番に生成します。

Piの `reasoning`、Chatの `reasoning_effort` を受け付け、OpenCodeのモデルカタログに該当するvariantがあれば、そのプロバイダー固有の推論設定を使います。`big-pickle` など推論強度のvariantがないモデルはモデル既定動作になります。推論本文とトークン使用量はSSEにも返します。`include`、キャッシュキー・保持期間、service tierなどのクライアント付帯情報は受け付けますが、上流でのキャッシュ期間・サービス階層・暗号化推論データの取得は指定しません。

function toolsのJSON Schemaは、要求ごとのローカルMCPからOpenCodeへ渡します。OpenCode側では `ocpool_` の名前空間が付き、クライアントへ返す際に元のツール名へ戻します。実際のツール実行はPiなどのクライアント側です。OpenCodeの権限要求でネイティブ実行を止め、モデルが生成したcall IDと引数をそのまま返します。要求終了時にMCPを切断し、次の要求へツール定義を持ち越しません。

上流のツール選択はOpenCode標準のautoです。`StructuredOutput` は使いません。`tool_choice:required` / 名前指定は公開する定義と指示で制約し、返された呼び出しを検証します。モデルが従わない場合はエラーを返すため、上流での強制選択と同等ではありません。`parallel_tool_calls:false` は1件に制約し、返された件数を検証します。テキストは生成中に配信し、ツール呼び出しはネイティブのターンが完了してから返します。独自文法のcustom tools、画像・音声は未対応です。

会話の履歴はロール・tool call IDを含むJSON形式でOpenCodeへ渡すため、他社APIとトークン消費・応答の完全一致は保証しません。Responsesの過去のreasoning項目は受け付けますが、プロバイダー固有の暗号化推論データの再利用は行いません。OpenCode内蔵のファイル操作・シェル等で権限確認が発生した場合は拒否し、クライアントへエラーを返します。

## 数百アカウントの扱い

登録数と常駐プロセス数を分離しています。標準設定は同時稼働4、待機リクエスト128、待機本文合計64 MiB、1要求2 MiBです。未使用のOpenCodeは5分で停止し、利用期間や要求数の目安に達したアカウントから待機候補へ入れ替えます。

管理状態はSQLite/WALに保存し、モデル一覧・アカウント一覧の表示で全プロファイルを起動しません。1,000件の管理データ、並列要求時の上限、登録処理との実行枠共有をテストしています。1,000件の実アカウント登録を行ったわけではありません。

## 出口とIP制限

「出口 / IP」画面で直通、専用WARP、HTTP/SOCKSプロキシ、relayを管理できます。アカウントの数と出口の数は独立しています。同じ公開IPを共有する出口は、同じ休止状態を参照します。

専用WARPはDocker内で動かし、Windows側のWARP接続・ネットワーク設定は変更しません。

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/setup-warp.ps1 -Rebuild
node scripts/configure-warp-egress.mjs
```

設定後はWARPを優先します。通常回線への切り替えは管理画面の「直通へのフォールバック」で選択します。モデル要求は実際のOpenCodeが生成したものを、そのまま出口へ中継します。OAuthアカウントの `/inference/*` とZenの `/zen/*` に対応し、認証情報・組織ヘッダーも維持します。

429は種類に応じて処理します。IP制限は出口IP全体、判断できない制限は出口とモデルの組で休止し、`Retry-After` をSQLiteへ保存します。並行要求が短い休止時間で上書きすることはありません。契約・アカウントの上限は別の出口へ再送しません。SSEが始まった後は出口層で要求を再送せず、切り替えは配信が終わるまで待ちます。上流の429はクライアントへ429として返し、OpenCode内部の長い再試行待ちを止めます。

現在の実機ではWARPのOpenCode向け公開IPは `104.28.238.115`。再接続、新しい登録2回、WireGuardへの変更でも、このIPは変わりませんでした。新しいIPの取得は保証できず、同じIPなら休止を維持します。詳細と検証結果は [専用WARP](ops/warp/README.md) と [経路比較](docs/opencode-route-options.md) を参照してください。

### Surfsharkの複数WireGuard接続

提供された `wg.conf` を使い、全カタログ候補を1つのDocker内WireGuardプールで管理します。CSV一覧をSurfshark公式の公開鍵と現在のDNSへ照合し、候補をランダムに選び、ハンドシェイクと対象サイト向けの実通信で確認します。

```powershell
node scripts/surfshark-catalog.mjs --wg-config wg.conf --probe-count all
node scripts/setup-surfshark.mjs --pool --rebuild --connections 4
```

全280個のcurrent-DNS WireGuard候補を1つの `ocpool-surfshark` コンテナーで管理し、既定では4つの接続を維持します。`--connections` で2〜32接続へ変更できます。280候補すべてがランダムな交換先になり、280本を同時起動する構成ではありません。OCPoolからは1つの `sticky` 論理出口として使用し、直通fallbackは無効です。

利用制限と接続の交換はDocker内で管理します。制限・故障がある接続の応答完了を待って交換し、ほかの正常な接続は通信を続けます。公開IPの休止状態はファイルへ保存するため、コンテナーを再作成しても残ります。アカウント・契約の上限ではVPNを交換しません。

「出口 / IP」画面から、同じサーバーへの再接続と別サーバーへの切り替えを操作できます。同じParisサーバーへの再接続では今回はIPが変わりませんでした。IPが変わらない場合は、同じIPの休止状態を維持します。IP以外の契約・モデルの制限がなくなることを保証するものではありません。

詳細: [Surfsharkの設定と実測](docs/surfshark.md)、[WireGuard relay](ops/wireguard/README.md)。

## 保存場所

- `.state/server/pool.db`: アカウント、ジョブ、APIキーのハッシュ、レスポンス、要求結果
- `.state/server/admin-key.txt`: 管理画面のログインキー
- `.state/server/egress.json`: 出口設定・relayの共有キー
- `.state/warp/control-token.txt`: 専用WARPの管理キー
- `.state/surfshark/base.conf`: 提供されたWireGuard設定の保護されたコピー
- `.state/surfshark/pool.json`: 全カタログを持つプールrelayの公開メタデータ
- `.state/surfshark/pool-state/limits.json`: Docker内の出口IP・モデルの休止期限
- `.state/surfshark/fleet.json`: 移行互換の稼働VPNメタデータ
- `.state/email-bridge.json`: Email Workerの接続先とAPIトークン
- `.state/onboarding/<account-id>/`: 登録情報、CloakBrowser、OpenCodeの認証・DB

Windowsでは起動時に `.state` のACLを現在の利用者だけに設定します。Unixではユーザー専用権限を使います。ブラウザー画像はジョブ実行中のメモリに保持し、一般のAPIキーからはアクセスできません。

## 検証

```powershell
go test ./internal/app
go vet ./...
npm.cmd test
node --test scripts/oauth-headless.test.mjs scripts/parameters.test.mjs
node scripts/inspect-dashboard.mjs
node scripts/smoke-server.mjs
node scripts/smoke-generation-options.mjs
node scripts/smoke-pi.mjs
node scripts/smoke-pi.mjs --coding-tools
node scripts/smoke-pi.mjs --auto-tools
node scripts/inspect-egress.mjs
node --test scripts/surfshark-catalog.test.mjs scripts/setup-surfshark.test.mjs
node scripts/smoke-surfshark.mjs
```

`smoke-server.mjs` は実際にモデルを呼び出します。`smoke-web-creator.mjs` は実アカウントを1件登録します。後者は通常のユニットテストには含めません。

`smoke-pi.mjs` はインストール済みPiの送信・解析実装を使い、両APIで推論・ツール呼び出し・結果の再送を検証します。必要なら `OCPOOL_PI_AI_DIR` でPi AIの `dist` ディレクトリを指定できます。

`--coding-tools` はOpenCode内蔵ツールと同じ `read` / `bash` を宣言し、指定した `read` をクライアントへ返すことと `parallel_tool_calls:false` を検証します。`--auto-tools` はPi標準の自動選択を検証します。どちらもツール結果にはfixtureを返し、実ファイルやシェルを操作しません。

実際のWARP制限が記録されているときは `node scripts/smoke-egress-rate-limit.mjs` で、WARPのみの設定で429を速やかに返すことを確認できます。制限を発生させるための連続生成は行いません。

この環境ではCコンパイラーがないため、`go test -race` は実行できていません。並列処理の通常テストと `go vet` は実行済みです。

詳細: [設計](PLAN.md)、[登録・OAuth](docs/onboarding.md)、[管理サーバーの検証](docs/server-verification.md)。

APIが完全な透過転送ではない理由と、Museの `tool_choice` エラーの原因は [OpenCodeの経路解析](docs/opencode-routing-analysis.md) を参照してください。
