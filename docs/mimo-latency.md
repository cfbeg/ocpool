# MiMoの待ち時間とストリーミング

`opencode/mimo-v2.6-flash-free` はOpenCodeのOpenAI互換providerを使い、推論を `reasoning_content` で返す。短い合成要求を128トークン・90秒以内に制限して確認したところ、SSEは13.98秒、通常応答は4.39秒で成功した。一方、過去の記録には131秒の成功、約30秒での `context canceled`、上流の `Endpoint is unavailable` がある。

中断した要求では、上流のHTTP 200を受けてから応答終了まで待っていた。中断元は記録から確定できないが、従来は最初の推論・本文deltaが来るまでクライアントへSSEヘッダーを送らなかったため、長い待ち時間を切断と判断するクライアントがあり得る。

Chat Completionsは入力検証後すぐにassistantのrole chunkを送り、Responsesは `response.created` を送る。生成を待つ間は10秒ごとに `: keep-alive` コメントを送る。コメントとJSONイベントは同じロックでフレーム全体を直列化し、書き込み失敗時は生成を中止する。ハンドラー終了前にkeepalive処理を停止し、終了を待つ。

ストリーミングを開始した後はHTTPステータスを変更できない。上流エラーはHTTP 200のSSE内で返し、Chatは型・code・status付きの `error` イベントと `[DONE]`、Responsesは `response.failed` を返す。入力エラー・存在しない過去レスポンスはSSE開始前のHTTP 400・404のまま。通常応答では上流429などをHTTPステータスで返す。

この変更は待機中の接続を維持するもので、上流のモデル提供状況や推論時間そのものを短縮するものではない。実測の詳細は `.state/research/mimo-findings.json` に保存した。

反映後の実機確認では、ChatのSSEヘッダーが60ms、Responsesが17msで届き、本文は約2.5秒と2.7秒で `MIMO_WG_POOL_OK` を返した。両方とも正常に終端イベントを受信した。記録は `.state/server/mimo-pool-result.json`。これは短い要求の確認で、過去の30秒切断の中断元が確定したことや、すべての要求が同じ時間で完了することを示さない。
