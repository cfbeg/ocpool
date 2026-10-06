# クライアントツールへの引き渡し

2026-10-06。MiMoのResponses要求で `the model requested an internal tool` が繰り返された。失敗したOpenCodeセッションの読み取り専用WAL調査では、クライアントの `ocpool_bash` と内蔵の `bash` が両方公開され、モデルが後者を選んでいた。ゲートウェイは実行を拒否していたが、その時点でAPI要求全体をエラー終了していた。

通常のプロバイダーではセッションの権限を `* deny` とし、その要求に含まれるMCPツールの正確な別名だけを `ask` で公開する。通常チャットと `tool_choice:none` ではクライアントツールを公開しない。会話のfork後も要求ごとの権限を設定し直す。OpenCodeの最後に一致した権限ルールが優先され、deny対象のツールはモデルへ渡す定義から除外される。[公式の権限仕様](https://opencode.ai/docs/permissions/)

OpenCodeの無料経路では、実際のCLIで内蔵ツールを除外した要求がHTTP 403 `FreeTierError` になった。サーバー側の具体的な検証条件は未確定であるため、この経路はCLIの通常のツール定義を保持し、すべての実行要求を `ask` にする。認証・CLIヘッダー・ツール定義の偽装や、直接HTTPへの切り替えは行っていない。

宣言されたクライアントの `ocpool_*` ツールが選ばれた場合、実行前の権限要求を拒否し、元のcall IDと引数をクライアントへ返す。クライアントがツールを実行し、その結果を次の要求で返す。

内蔵ツールが誤って選ばれた場合、その実行を拒否し、現在のOpenCode応答が終了してから同じセッションで修正を促す。修正は最大2回、最初の要求を含め最大3回で終了する。内蔵 `question`・`plan_exit` が直接起こす質問も拒否し、端末入力を待ち続けない。既存のクライアントツール呼び出しがある場合は再試行せず、その呼び出しを引き渡す。

修正前に配信された本文と推論内容を保持し、各段階の使用量を合算する。次の段階には元の出力トークン上限から消費済み量を引いた予算だけを渡し、予算が尽きた場合は `length`／`max_output_tokens` で終了する。キャンセル・上流403・429などはツール修正の対象にしない。

Goの両APIテストでは本文・ツールへの復帰、fork、質問拒否、最大試行回数、トークン予算と配信済み本文の保持を検証した。実際のOpenCode CLIとローカル模擬上流でも、内蔵bashの拒否後にツール結果の履歴を保って復帰することを確認した。記録は `.state/research/tool-scope-native-test.log` と `tool-correction-native-test.log`。

MiMoの実機確認では、通常のChat・Responses要求と、インストール済みPiによる `tool_choice:auto` のクライアントread呼び出し・結果の往復が成功した。Pi側は `reasoningEffort:max` を送信した。この値がモデルの推論強度を変更したことを保証する試験ではない。実機記録は `.state/server/mimo-pool-result.json` と `.state/server/pi-auto-tools-result.json`。
