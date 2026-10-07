# 互換APIのリクエスト変換

`POST /v1/chat/completions` と `POST /v1/responses` は、受信したJSON全体と各トップレベルの `json.RawMessage` を保持し、型付きフィールドを既存のOpenCodeアダプターへ渡します。元のJSONを上流へそのまま送ることはありません。

```text
受信JSON → 元のJSONを保持 → 対応項目を判定 → 型付きリクエスト
                              ↓                   ↓
                         未対応なら422       OpenCode Session.Prompt
```

保持期間はHTTP要求の終了までです。SSEではストリームが閉じるまでリクエストコンテキストに残します。生JSON・`client_metadata`・利用者識別子をログやSQLiteへ保存せず、モデル入力・OpenCodeのパラメーターファイルへ混ぜません。Responsesの標準 `metadata` は例外で、応答に返し、`store:true` ならその応答と一緒に保存します。

## 経路と対応表

現在実装されている経路は全モデルとも `opencode-session` です。モデル名からネイティブResponses対応を推測しません。上流プロトコルの選択はOpenCodeが行います。この変更でChat/Responses/Anthropicへの直接中継が追加されるわけではありません。

| 指定 | 現在の処理 |
| --- | --- |
| `model`、テキスト会話、instructions | 既存のSession.Prompt入力へ変換 |
| 会話の名前、assistantの `phase` | 会話JSONに保持。名前付きsystem/developerはシステム文へ反映 |
| `temperature`、`top_p`、トークン上限 | OpenCodeパラメータープラグインへ変換。上限はモデル上限以下 |
| `reasoning.effort` / Chatの `reasoning_effort` | モデルのvariantへ変換。variantがなければ既定動作。`mapped-model-dependent` と報告 |
| `reasoning.summary` | 既存のモデル依存フックへ渡す。OpenAI/Azure形式以外では適用されないため `mapped-model-dependent` と報告 |
| function tools | MCPへ変換。JSON Schemaの未知キーワードと大きな整数も保持 |
| `tool_choice`、`parallel_tool_calls` | 既存の定義・指示・結果検証による変換。強制指定は `mapped-validated`。ネイティブ強制選択とは異なる |
| `client_metadata`、`user`、`safety_identifier` | `retained-local`。要求中だけ保持し、モデルには送らない |
| Responsesの `metadata` | 応答に含め、`store:true` のときだけ応答DBにも保存 |
| Chatの `metadata` | 要求中だけローカル保持 |
| `prompt_cache_key` | APIキーID・モデル・値をハッシュ化し、利用可能なアカウントへの固定に利用 |
| Responsesの `store` / `previous_response_id` | 既存の30分保存・同じAPIキーによる取得・同じアカウント上のセッションfork |
| Chatの `store:false` / `null` | 保存しない。`true` は未対応として422 |
| `text.format.type:"text"` / Chatの `response_format.type:"text"` | 通常テキスト出力として受理 |
| `text.verbosity`、構造化JSON出力、functionの `strict:true` | 未対応として422 |
| 非空の `include`、非空の `encrypted_content` を含む推論履歴 | ネイティブ暗号化推論・logprobs等を取得・再利用できないため422 |
| 過去のreasoningのsummary/content、Chatのreasoning履歴、annotations/logprobs | 要求中だけローカル保持。生成用の履歴には再利用しない |
| `prompt_cache_retention`、非空の `prompt_cache_options` | 上流のキャッシュ設定を再現できないため422 |
| `service_tier:"auto"` | 上流既定動作。その他の明示的な階層は422 |
| 空の `access_programs`、`background:false` | 既定動作。それ以外は422 |
| `truncation`、未知のフィールド、未知の入れ子フィールド | 対応する変換がないため422 |

`prompt_cache_key` はアカウント選択のヒントとしての変換です。ネイティブのキャッシュキーやTTLを設定せず、キャッシュヒットも保証しません。同じキーの要求は選択したアカウントが使用中なら通常のキュー時間内で待機します。無効・制限中になったアカウントは候補から除外し、再選択します。`previous_response_id` のアカウント指定が常に優先され、キャッシュキーを理由に会話セッションを共有しません。対応表の「要求中だけ保持」は、再起動後の再利用や別の要求への引き継ぎを意味しません。

## 診断とエラー

通常応答・SSEとも、開始前に次のヘッダーを付けます。値は含めません。

```text
X-OCPool-Adapter-Route: opencode-session
X-OCPool-Adapter: client_metadata=retained-local, input=mapped, model=mapped, prompt_cache_key=mapped-account-affinity, store=mapped-local-store
```

フィールド名はURLエンコードされます。入れ子は `text.verbosity`、配列は `include[0]` のように表し、ヘッダーではそれぞれ `text.verbosity`、`include%5B0%5D` になります。ヘッダーは4 KiB以下に制限し、収まらない場合は `X-OCPool-Adapter-Truncated: true` も返します。ヘッダーはアダプターの処理方針であり、上流モデルがすべての設定を適用したことの証明ではありません。

未対応項目があると、アカウント取得・OpenCode起動・SSE開始の前にHTTP 422を返します。1個ずつ直す必要がないよう、検出した項目を一度に列挙します。

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "unsupported_parameter",
    "param": "text.verbosity",
    "message": "The OpenCode session adapter cannot preserve the requested semantics; see unsupported_parameters.",
    "unsupported_parameters": [
      {
        "param": "text.verbosity",
        "action": "unsupported",
        "reason": "No text verbosity mapping is available on the session route."
      }
    ],
    "adapter": {
      "route": "opencode-session",
      "fields": [
        { "param": "client_metadata", "action": "retained-local" },
        { "param": "input", "action": "mapped" },
        { "param": "text.verbosity", "action": "unsupported", "reason": "No text verbosity mapping is available on the session route." }
      ]
    }
  }
}
```

不正な型・複数JSON値・重複キー・過度な入れ子は400、2 MiB超の本文は413です。管理APIは従来どおり未知フィールドを拒否する厳格なデコーダーを使います。

## クライアントの設定

`client_metadata` だけを理由とした400は解消されます。ただし、Codex/Piなどが `include:["reasoning.encrypted_content"]`、`text.verbosity`、キャッシュ保持期間等を自動送信する構成では、今回のアダプターは422を返します。エラー一覧を確認し、クライアント側で必要な機能を選んでください。中継側でそれらを黙って削除したり、満たせない機能を成功扱いにはしません。要求にその機能が必須なら、対応する下流アダプターが必要です。

現在の実装で受理するResponses要求の例:

```json
{
  "model": "opencode/big-pickle",
  "input": "こんにちは",
  "client_metadata": { "client": "example" },
  "prompt_cache_key": "conversation-1",
  "text": { "format": { "type": "text" } },
  "store": false,
  "stream": true
}
```

機能の意味は[OpenAI公式Responses API定義](https://developers.openai.com/api/reference/cli/resources/responses/methods/create)を参照してください。このページの対応表はOCPoolの実装範囲を示します。

## 検証

`request_adapter_test.go` は、元JSONと数値の保持、入れ子の未対応項目、生成前の422、SSE/通常応答の診断、SDKへの送信内容とパラメーターファイルへのメタデータ混入防止、Responsesの保存・APIキー間の分離、アカウント固定と休止中の再選択を検証します。上流はローカルの模擬OpenCode HTTPサーバーで、テストは実アカウントを消費しません。
