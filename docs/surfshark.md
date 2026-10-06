# SurfsharkのWireGuard出口プール

提供された `wg.conf` と[サーバー一覧CSV](https://raw.githubusercontent.com/mthcht/awesome-lists/refs/heads/main/Lists/VPN/SurfSharkVPN/surfshark_vpn_servers_domains_and_ips_list.csv)を使い、1つのDockerコンテナー内で複数のWireGuard接続と利用制限を管理する。元の設定ファイルは変更せず、秘密鍵・tokenをログや公開メタデータへ出さない。

## カタログ

`scripts/surfshark-catalog.mjs` はCSVのホストを[Surfshark公式API](https://api.surfshark.com/v4/server/clusters)の `connectionName` と `pubKey` へ対応付け、現在のDNSが返す公開IPv4を全件保存する。別サーバーへ変更するときは、そのサーバーのpeer公開鍵を使用する。CSVの1194はOpenVPNのポートなので、WireGuardには51820を使用する。

2026-10-06の再取得では、CSV282行から141ホストを照合し、現在のDNSが返す280エンドポイントを登録した。全280候補をICMP確認し、278候補が応答した。pingの応答はVPNの接続成功とは別に扱い、応答がない候補も削除しない。実際のWireGuardハンドシェイクと `opencode.ai/cdn-cgi/trace` が成功した接続だけを推論に使う。

```powershell
node scripts/surfshark-catalog.mjs --wg-config wg.conf --probe-count all
```

- `.state/surfshark/servers.json` は全候補のホスト、接続先IP、peer公開鍵、地域、DNS・ICMP確認結果を保存する。
- `.state/surfshark/catalog-metadata.json` は取得元と日時、入力・出力件数を保存する。`catalog_complete:true` は検証済みカタログ全件を保持した意味で、CSV内の古いIPが現在も利用可能なことを示さない。
- `--probe-count 32` はICMP確認だけを32件に絞る。保存するカタログは全件のまま。

カタログのVPN接続先IPと、トンネルを通った後の実際の出口IPは別の値である。同じサーバーに再接続しても、出口IPが同じなら同じ制限状態を引き継ぐ。

## 1コンテナー内の複数接続

`scripts/setup-surfshark.mjs` は1つの `ocpool-surfshark` コンテナーを作り、管理APIを `127.0.0.1:18100` に公開する。全カタログを読み取り専用でマウントし、既定では4つのWireGuard接続を維持する。`--connections` で2〜32接続へ変更できる。

```powershell
node scripts/setup-surfshark.mjs --pool --rebuild --connections 4
node scripts/setup-surfshark.mjs --pool --configure-only
```

4つは同時に維持する接続数であり、カタログの候補数ではない。現在の280候補はすべてランダムな接続先選択・交換の対象となる。候補数が数千に増えても、同時接続数分のインターフェイスだけを維持する。280本のトンネルを同時に起動する構成ではない。

各接続は `wg0`、`wg1` など別のインターフェイス、経路表、HTTP transportを使う。推論TCPとDNSを該当インターフェイスへ明示的に結び付け、VPNが停止したときは通常回線へのフォールバックを禁止する。管理APIの応答と暗号化されたWireGuard UDPはDockerの通常ネットワークを使い、ホストのVPNや経路を変更しない。

セットアップは少なくとも2つの接続で新しいハンドシェイクと対象サイト向けtraceを確認し、実測の出口IPが異なることを確認してからOCPoolへ登録する。OCPool上では1つの `sticky` なVPN出口として設定し、直接通信へのフォールバックを無効化する。VPNの交換と利用制限はDocker側で管理するため、管理サーバーの自動交換は無効にする。

更新には `--rebuild` を使用する。既存の接続は新規通信を止めて実行中の応答を待ってから交換する。番号付きの旧コンテナーは新しいプールの設定成功後に停止・削除する。制御APIやtokenを確認できない稼働コンテナーは保持し、結果の `legacy.preserved` に記録する。旧構成は明示的な `--fleet` でのみ利用できる。

## 利用制限と交換

Docker内のプールは、実測の公開IP、モデル、上流の制限内容を使って利用できる接続を選ぶ。IP全体の制限とアカウント・契約の上限を区別し、アカウントの上限ではVPNを交換しない。`Retry-After` を保持し、同じ公開IPの制限を再接続で解除しない。

制限中・故障・重複IPの接続を候補から外し、全カタログからランダムに代替候補を選ぶ。接続を交換するときはその接続の応答完了を待ち、ほかの正常な接続は推論を続ける。途中のSSEを再送しない。接続先のpingだけで使用可能と判定せず、実際のハンドシェイクと出口IPを再確認する。

同じWGで1件目を配信中に2件目がIP制限の429を返した場合、そのIPへの新規要求を止め、2件目を別の正常なWGで最大4回の総試行まで再送する。1件目は元のWGへ固定し、本文の終了またはクライアント切断までリースを保持する。そのWGの交換はリースが0になってから行う。再試行先が故障した場合は元の429と `Retry-After` を保持する。すべての候補がモデル単位の休止中なら待ち時間付き429、正常な接続がない場合は503となる。

長い配信中でもハンドシェイクを読み取り専用のprobeで更新し、正常な接続を古いキャッシュだけで除外しない。各接続のTCP・DNSは専用インターフェイスへ固定され、別接続の交換で経路表や接続先を変更しない。同じホストやpeer公開鍵を複数の稼働接続へ同時に割り当てない。

休止状態は `.state/surfshark/pool-state/limits.json` に保存する。このディレクトリーを `/var/lib/ocpool` へマウントし、`OCPOOL_VPN_STATE_FILE=/var/lib/ocpool/limits.json` を設定するため、コンテナーを再作成しても休止期限は残る。

`GET /health` は代表接続の従来フィールドに加え、`pooled:true`、`candidate_count`、`pool_size`、`ready_count`、`cooling_count`、`slots` を返す。各slotにインターフェイス名、実測IP、接続先、ハンドシェイク、処理中件数、休止期限が含まれる。代表の `public_ip` はプール全体に1つのIPしかないという意味ではない。

## 保存場所と検証

tokenとenvファイルは `.state/surfshark` 内に保存し、Windowsでは利用者とSYSTEMにアクセスを制限する。tokenをDockerの引数やセットアップ結果へ出さない。`pool.json`、`fleet.json`、`setup-result.json` は公開接続メタデータを保存する。

```powershell
node --test scripts/surfshark-catalog.test.mjs scripts/setup-surfshark.test.mjs
go test ./...
go vet ./...
```

これまでの実機検証では、Paris、Jakarta、Valenciaの3つの独立コンテナーからMuse生成に成功した。Parisへの同一サーバー再接続では出口IPが変わらず、`changed:false` を返した。VPNインターフェイスの停止時は503となり、通常回線へ漏れなかった。これらは旧構成の記録で、今回の複数接続プールの稼働確認とは別に扱う。記録は `.state/server/surfshark-live-result.json`、`.state/surfshark/same-server-result.json`、`disconnect-proof.json` にある。

今回の実機では1つのコンテナー内の `wg0`〜`wg3` が、Kansas、Boston、Lagos、Berlinへ接続し、異なる4つの実出口IPと新しいハンドシェイクを確認した。管理画面でも接続別のIP・通信中件数・休止状態を確認した。同じWGでのSSE継続と2件目の429→予備WGへの再送、キャンセル、モデル単位休止、休止状態の再読込を回帰テストで検証し、Linux Dockerのrelayテストも成功した。

MiMoはChat・Responsesとも実生成に成功し、更新後はSSE開始が60ms・17ms、本文が約2.5秒・2.7秒だった。Piの両APIでもクライアントツール呼び出しと結果の往復を確認した。最終記録は `.state/server/egress-final-verification.json`、`.state/server/mimo-pool-result.json`、`.state/server/pi-auto-tools-result.json`。MiMoの待ち時間とSSEの通知仕様は [MiMoの検証](mimo-latency.md) に記録した。

参考: [SurfsharkのWireGuard Linux設定](https://support.surfshark.com/hc/en-us/articles/19262406140306-How-to-set-up-a-manual-WireGuard-reg-connection-on-Linux)、[GluetunのSurfsharkカタログ](https://github.com/qdm12/gluetun/blob/master/internal/provider/surfshark/updater/api.go)、[WireGuard relayの仕様](../ops/wireguard/README.md)。
