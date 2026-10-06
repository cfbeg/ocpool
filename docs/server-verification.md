# 管理サーバーの検証

2026-10-05〜2026-10-06（JST）。

## 確認済み

- Goサーバーの起動、管理キーからのCookieログイン、未認証アクセスの拒否。
- 既存の検証済みアカウントを取り込み、Webの一覧に表示。
- Webから新規登録ジョブを開始。同じジョブを再開し、登録 → 実メール → メール確認 → OAuth → モデル疎通確認まで完了。
- 登録専用CloakBrowserの画像をWebで表示し、Webからのキー入力を受付。
- ブラウザー操作用IPCのテストで、画像配信と実際のクリックによるボタン操作を確認。
- Web画面からジョブの絞り込み、設定保存、APIキーの発行・無効化を確認。
- Chat Completionsが `SERVER_OK` を返す。
- SSEが `STREAM_OK` と `[DONE]` を返す。
- `stream_options.include_usage` が有効な場合、使用量の最終イベントも返す。
- Responsesで記憶させた語を `previous_response_id` で継続して取得。
- PiにインストールされているSDKからResponses・Chat Completionsへ接続し、`reasoning` / `reasoning_effort`、推論SSE、function call、ツール結果を含む次の要求まで実機確認。
- `temperature`、`top_p`、`max_tokens`、`max_completion_tokens`、`stop:null`、`stop:[]`、チャンク境界をまたぐstop処理、`n=2`。実機の短いトークン上限で `length` も確認。
- OS既定ブラウザーを開かないdevice OAuthで既存アカウントを再認証し、保存後のネイティブ生成に成功。
- 中断されたジョブは後続の成功ジョブにより `resolved` / readyへ更新され、古いエラーを表示しない。
- 実ブラウザーで「再開後に完了」、再開ボタンの無効化、成功ジョブへのリンクを確認。完了後はブラウザーの起動待ち表示と操作欄を隠し、画像のポーリングも停止する。
- Piの `read` / `bash` というツール名を渡し、指定した `read` と `parallel_tool_calls:false` が両APIで機能することを確認。
- Pi標準の `tool_choice:auto` でも、クライアントへ `read` を返して結果を受け取り、回答まで完了することを両APIで確認。
- `muse-spark-1.3-contributor-free` のauto限定制約に対し、StructuredOutputによるrequiredの追加を撤去。MCP経由のツール定義で、Museの両APIのread呼び出しと結果往復を実機確認。
- 「出口 / IP」画面で設定編集・共有キーの非表示・疎通・再接続・モバイル表示をCloakBrowserで確認。編集途中の設定を更新で消さない。
- Docker内の専用WARPが `warp=on`、対象 `opencode.ai` で公開IP `104.28.238.115` を確認。WindowsのWARPは手動切断状態を維持。
- OAuthの実モデル要求が `/inference/openai/v1` を使うことを確認して中継対象へ追加。出口の実測カウンターと `FreeUsageLimitError` の429により、実際に専用WARPを通ったことを確認。
- その公開IPは既に無料枠の上限に達していた。同一IPの休止を解除せず、再要求へ429とRetry-Afterを19msで返した。WARP経由の成功した生成・実SSE完了は、上限により今回未確認。
- SurfsharkのWireGuardを3コンテナーで維持。OpenCode向け実出口IPはParis `85.204.70.104`、Jakarta `93.185.162.128`、Valencia `193.19.207.87`。各peerの公開鍵を公式データと照合し、最新ハンドシェイク・対象通信を確認。
- 上記3出口それぞれからMuseへ実通信し、通常生成を計6要求で確認。期待する `SURFSHARK_POOL_OK` を確認し、出口の200カウンターとも照合した。稼働記録は `.state/server/surfshark-live-result.json`。
- Parisへ同じサーバー再接続を1回実行し、ハンドシェイクは更新されたが同じIPで `changed:false`。コンテナー内でトンネルを停止するとfetch503、復帰後はhealthyを確認。ホストのVPN・ルーティングは変更していない。
- 1,000件の管理データでアカウント・ジョブのページング。
- 並列要求時のプロセス上限、workerの再利用、登録ジョブとの枠共有。
- APIキーの作成・無効化、管理CookieへのCSRF対策、秘密ファイルの非公開。
- レスポンス期限切れ時の清掃キューへの移動。
- Windows版のビルドと、Linux amd64版のクロスビルド。

## 検証中に修正した点

- OpenCodeのユーザーメッセージのイベントをSSE出力へ混ぜないよう、assistantのパートだけを対象にした。
- GoのアカウントIDに含まれる `_` を登録スクリプトでも許可し、同じアカウントのジョブを再開できるようにした。
- ブラウザー画像のblob URLを管理画面のCSPで許可し、画像のクリック座標を実ブラウザーの表示領域へ変換した。
- 起動時の既存プロファイル読み込みで、無効化状態や認証失効状態を上書きしないようにした。
- Responsesの継続で上流セッションをforkし、以前のレスポンスの会話状態を変更しないようにした。
- 再開元ジョブを `resumed_from` / `resolved_by` で関連付け、旧データも起動時に照合する。中断ジョブの完了復旧は `completed_run` が一致する実行だけを対象にする。
- OpenCodeのキャッシュ入力・推論出力を使用量の合計に含め、内訳も返す。
- テキストが空のトークン上限応答でもResponsesのSSE開始・項目・終了イベントを正しく出す。
- 初回プラグイン初期化がキュー待機制限に巻き込まれないよう、枠の確保後は要求全体の期限と60秒の起動制限で管理する。
- 要求単位でMCPを接続・切断し、クライアントツールの権限要求でネイティブ実行を止める。長いツール名は名前空間を含め64文字以内に収め、クライアントへは元の名前とネイティブcall IDを返す。切断失敗時はworkerを再利用しない。
- 出口層で同じIPの休止共有、Retry-Afterの永続化・最大値保持、次の出口の接続に失敗した場合の元の429保持、アカウント上限での切り替え禁止をテスト。
- SSE本文が終わるまで出口の処理中件数を保持し、切断時に上流も中止する。配信後の再送と処理中の設定書き換えを防ぐ。
- 出口が返したIP制限をGo側の生成処理へ通知し、OpenCode内部で1時間の再試行待ちになっても速やかに停止する統合テストを追加。背景のタイトル生成と本要求を区別する。

## 検証範囲

登録からの検証に使った実アカウントは2件。数百件の登録完了率、長期間のトークン更新、Linuxでの実行、全種類の外部ツール呼び出しは未検証。外部ツールはJSON Schemaのfunction形式に対応し、独自文法のcustom tools・画像・音声は未対応。

実機API検証の記録は `.state/server/pi-smoke-result.json`、`.state/server/pi-coding-tools-result.json`、`.state/server/pi-auto-tools-result.json`、`.state/server/generation-options-result.json`。Piテストはテスト用のツール結果をクライアントから返し、サーバーや利用者のファイル操作は実行しない。`big-pickle` は推論強度のvariantを公開していないため、effort指定は受け付けてモデル既定動作を使う。variantへの変換自体はOpenAI形式とAnthropic形式のfixtureでも検証する。

Goのrace detectorはCGO用Cコンパイラーがないため実行できていない。
