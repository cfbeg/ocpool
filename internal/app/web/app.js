const $ = (q, root = document) => root.querySelector(q);
const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const icons = {
  overview: "M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z",
  accounts:
    "M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2 M16 3a4 4 0 0 1 0 8 M22 21v-2a4 4 0 0 0-3-3.87 M13 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0",
  jobs: "M9 4h6v3H9z M8 5H5v16h14V5h-3 M8 12h8 M8 16h5",
  keys: "M15.5 7.5a5 5 0 1 1-2 8.5L8 21H3v-5l6-6 M16 8h.01",
  play: "M8 5l11 7-11 7z",
  egress: "M12 3v5 M5 17v4 M19 17v4 M5 13h14 M12 8v5 M2 13h6v4H2z M16 13h6v4h-6z",
  settings:
    "M12 8a4 4 0 1 1 0 8 4 4 0 0 1 0-8 M12 2v3 M12 19v3 M2 12h3 M19 12h3 M5 5l2 2 M17 17l2 2 M5 19l2-2 M17 7l2-2",
  server: "M3 3h18v7H3z M3 14h18v7H3z M7 6.5h.01 M7 17.5h.01",
  check: "M5 12l4 4L19 6",
  activity: "M3 12h4l3-8 4 16 3-8h4",
};
const icon = (name) =>
  `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-linecap="round" stroke-linejoin="round">${(
    icons[name] || icons.activity
  )
    .split(" M")
    .map((p, i) => `<path d="${i ? "M" : ""}${p}"/>`)
    .join("")}</svg>`;
const labels = {
  ready: "利用可能",
  completed: "完了",
  resolved: "再開後に完了",
  previous_error: "以前のエラー",
  new: "未登録",
  queued: "待機中",
  running: "実行中",
  waiting: "確認待ち",
  needs_attention: "確認が必要",
  failed: "失敗",
  cancelled: "停止",
  interrupted: "中断",
  login_required: "再認証が必要",
  exhausted: "利用枠なし",
  browser_verified: "メール確認済み",
  oauth_verified: "OAuth認証済み",
  signup_action_required: "登録の確認待ち",
  verification_pending: "メール待ち",
  signup_submitting: "登録送信中",
  preparing: "準備中",
  waiting_for_browser_confirmation: "ブラウザーの確認待ち",
  waiting_for_verification_email: "確認メール待ち",
  oauth_starting: "OAuthを開始",
  oauth_device_issued: "デバイス認可",
  oauth_saved_by_opencode: "OAuth保存済み",
  oauth_saved_to_profile: "OAuth保存済み",
  sdk_probe_starting: "モデル疎通確認",
  busy: "生成中",
  idle: "待機",
  starting: "起動中",
  draining: "停止待ち",
  creator: "登録処理",
};
const badge = (status) =>
  `<span class="badge ${esc(status)}">${esc(labels[status] || status)}</span>`;
const time = (unix) =>
  unix
    ? new Date(unix * 1000).toLocaleString("ja-JP", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
      })
    : "—";
const brand = `<div class="brand"><span class="logo"><i></i><i></i><i></i><i></i></span><span class="brand-name">OCPool</span></div>`;
const views = {
  overview: "概要",
  accounts: "アカウント",
  jobs: "登録ジョブ",
  keys: "APIキー",
  play: "プレイグラウンド",
  egress: "出口 / IP",
  settings: "設定",
};
let view = "overview",
  overview = {},
  accountPage = { items: [], total: 0 },
  jobs = [],
  jobPage = { items: [], total: 0 },
  jobFilter = { status: "", offset: 0 },
  keys = [],
  settings = {},
  egress = { config: {}, routes: [], events: [] },
  egressDraft,
  egressDirty = false,
  egressActions = new Set(),
  filter = { q: "", status: "", offset: 0 },
  modalTimer,
  browserTimer,
  frameURL,
  frameSize = { width: 1280, height: 720 },
  busy = false;
async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...options.headers },
  });
  if (response.status === 401) {
    showLogin();
    throw new Error("ログインが必要です");
  }
  const data = response.status === 204 ? null : await response.json();
  if (!response.ok) {
    const error = new Error(data?.error?.message || "操作に失敗しました");
    error.status = response.status;
    throw error;
  }
  return data;
}
function toast(message, error = false) {
  $(".toast")?.remove();
  const el = document.createElement("div");
  el.className = `toast${error ? " error" : ""}`;
  el.textContent = message;
  document.body.append(el);
  setTimeout(() => el.remove(), 6000);
}
function act(fn) {
  return async (event) => {
    try {
      await fn(event);
    } catch (error) {
      toast(error.message, true);
    }
  };
}
function showLogin(error = "") {
  clearInterval(browserTimer);
  $("#app").innerHTML =
    `<div class="login-screen"><div class="login-card">${brand}<h1>管理画面にログイン</h1><p>アカウントと登録ジョブを、ひとつの場所で。</p><form id="login-form"><input id="login-key" type="password" autocomplete="current-password" placeholder="管理キー" required><button class="primary">ログイン</button></form><div class="error-text">${esc(error)}</div><p><small>サーバーが作成した access-url.txt のリンクからもログインできます。</small></p></div></div>`;
  $("#login-form").onsubmit = async (event) => {
    event.preventDefault();
    try {
      await api("/api/login", {
        method: "POST",
        body: JSON.stringify({ key: $("#login-key").value }),
      });
      await boot();
    } catch (error) {
      showLogin(error.message);
    }
  };
}
function shell() {
  $("#app").innerHTML =
    `<div class="layout"><aside class="sidebar">${brand}<div class="nav-caption">WORKSPACE</div><nav class="nav">${Object.entries(
      views,
    )
      .map(
        ([id, name]) =>
          `<button data-view="${id}" class="${view === id ? "active" : ""}">${icon(id)}<span>${name}</span></button>`,
      )
      .join(
        "",
      )}</nav><div class="sidebar-bottom"><div class="online"><i class="dot"></i>サーバー接続中</div><div>OpenCode account workspace</div><button class="ghost small" id="logout">ログアウト</button></div></aside><main class="main"><header class="topbar"><div class="breadcrumb">Workspace &nbsp; / &nbsp; <strong id="crumb">${views[view]}</strong></div><div class="top-right"><span>OpenAI compatible API</span><span class="badge ready">● &nbsp; Connected</span><div class="avatar">OP</div></div></header><div class="content" id="content"></div></main></div>`;
  document
    .querySelectorAll("[data-view]")
    .forEach(
      (button) => (button.onclick = () => navigate(button.dataset.view)),
    );
  $("#logout").onclick = act(async () => {
    await api("/api/logout", { method: "POST", body: "{}" });
    showLogin();
  });
}
async function navigate(next) {
  if (next === "egress" && view !== next) {
    egressDraft = undefined;
    egressDirty = false;
  }
  view = next;
  document
    .querySelectorAll("[data-view]")
    .forEach((el) => el.classList.toggle("active", el.dataset.view === view));
  $("#crumb").textContent = views[view];
  await refresh();
}
async function refresh() {
  overview = await api("/api/overview");
  if (view === "accounts")
    accountPage = await api(
      `/api/accounts?q=${encodeURIComponent(filter.q)}&status=${encodeURIComponent(filter.status)}&limit=50&offset=${filter.offset}`,
    );
  if (view === "jobs") {
    jobPage = await api(
      `/api/jobs?limit=50&offset=${jobFilter.offset}&status=${encodeURIComponent(jobFilter.status)}`,
    );
    jobs = jobPage.items;
  }
  if (view === "keys") keys = await api("/api/keys");
  if (view === "settings") settings = await api("/api/settings");
  if (view === "egress") {
    egress = await api("/api/egress");
    egressDraft ??= copyEgressConfig(egress.config);
    if ($("#egress-form")) {
      renderEgressStatus();
      return;
    }
  }
  render();
}
function heading(title, description, actions = "") {
  return `<div class="page-head"><div><div class="eyebrow">${views[view].toUpperCase()}</div><h1>${title}</h1><p class="sub">${description}</p></div><div class="actions">${actions}</div></div>`;
}
function render() {
  const el = $("#content");
  if (!el) return;
  const counts = overview.accounts || {};
  const total = Object.entries(counts).reduce(
    (sum, [k, n]) => sum + (k === "archived" ? 0 : n),
    0,
  );
  const active = (overview.runtimes || []).length;
  if (view === "overview") {
    el.innerHTML =
      heading(
        "アカウントの稼働状況",
        "登録からモデルの利用まで、ここから管理できます。",
        '<button class="primary" id="add-account">＋ アカウントを登録</button>',
      ) +
      `<div class="cards"><div class="card"><div class="card-label">登録アカウント ${icon("accounts")}</div><div class="card-value">${total}<small> accounts</small></div><div class="card-foot">保存したアカウントの合計</div></div><div class="card"><div class="card-label">利用可能 ${icon("check")}</div><div class="card-value">${counts.ready || 0}</div><div class="card-foot green">認証・疎通確認済み</div></div><div class="card"><div class="card-label">稼働プロセス ${icon("server")}</div><div class="card-value">${active}<small> / ${overview.settings?.max_workers || 4}</small></div><div class="card-foot">必要なアカウントだけ起動</div></div><div class="card"><div class="card-label">24時間のリクエスト ${icon("activity")}</div><div class="card-value">${overview.requests_24h || 0}</div><div class="card-foot">成功 ${overview.success_24h || 0}件 · 処理中 ${overview.pending || 0}件</div></div></div><div class="grid-two"><section class="panel"><div class="panel-head"><h2>実行枠</h2><span class="muted">LIVE</span></div><div class="panel-body">${active ? overview.runtimes.map((r) => `<div class="slot"><div><div class="name">${esc(r.account_id)}</div><div class="slot-id">${r.requests} requests${r.pid ? " · PID " + r.pid : ""}</div></div><div class="actions">${badge(r.state)}<button class="small" data-stop="${esc(r.account_id)}">停止</button></div></div>`).join("") : `<div class="empty">${icon("server")}現在、稼働中のプロセスはありません。<br>APIリクエストや登録ジョブに応じて起動します。</div>`}</div></section><section class="panel"><div class="panel-head"><h2>プールの容量</h2></div><div class="panel-body"><div class="card-value">${active}<small> / ${overview.settings?.max_workers} slots</small></div><div class="progress-track"><div class="progress-fill" style="width:${Math.min(100, (active / (overview.settings?.max_workers || 4)) * 100)}%"></div></div><p class="sub">アカウント数と同時稼働数は別々に管理します。未使用のプロセスは自動で停止します。</p><button class="small" id="to-settings" style="margin-top:20px">実行枠を設定 →</button></div></section></div><section class="panel"><div class="panel-head"><h2>利用可能なモデル</h2><span class="muted">${overview.models?.length || 0} models</span></div><div class="panel-body"><div class="model-list">${(overview.models || []).map((m) => `<code>${esc(m)}</code>`).join("") || '<span class="muted">アカウントの疎通確認後に表示されます。</span>'}</div></div></section>`;
    $("#add-account").onclick = addAccount;
    $("#to-settings").onclick = () => navigate("settings");
    el.querySelectorAll("[data-stop]").forEach(
      (b) =>
        (b.onclick = act(async () => {
          await api(`/api/runtimes/${b.dataset.stop}/stop`, {
            method: "POST",
            body: "{}",
          });
          toast("停止を要求しました");
          await refresh();
        })),
    );
  }
  if (view === "accounts") {
    el.innerHTML =
      heading(
        "アカウント",
        "認証状態、利用可能モデル、稼働状況をまとめて管理。",
        '<button id="import-accounts">既存アカウントを取り込む</button><button class="primary" id="add-account">＋ アカウントを登録</button>',
      ) +
      `<section class="panel"><div class="toolbar"><input id="account-search" placeholder="名前・メール・タグで検索" value="${esc(filter.q)}"><select id="account-filter"><option value="">すべての状態</option>${["ready", "new", "oauth_verified", "signup_action_required", "login_required", "exhausted"].map((s) => `<option value="${s}" ${filter.status === s ? "selected" : ""}>${labels[s]}</option>`).join("")}</select><button id="search">検索</button><span class="spacer"></span><button class="small" id="bulk-enable">選択を有効化</button><button class="small" id="bulk-disable">選択を停止</button></div><div class="table-wrap"><table><thead><tr><th><input type="checkbox" id="select-all"></th><th>アカウント</th><th>状態</th><th>タグ</th><th>リクエスト</th><th>最終利用</th><th>操作</th></tr></thead><tbody>${accountPage.items.map((a) => `<tr><td><input class="account-select" type="checkbox" value="${esc(a.id)}"></td><td><div class="name">${esc(a.name)}</div><div class="email">${esc(a.email || a.id)}</div></td><td>${badge(a.status)}${!a.enabled ? ' <span class="tag">無効</span>' : ""}${a.cooldown_until > Date.now() / 1000 ? ' <span class="tag">休止中</span>' : ""}</td><td><div class="tags">${a.tags.map((t) => `<span class="tag">${esc(t)}</span>`).join("") || "—"}</div></td><td>${a.requests}</td><td class="muted">${time(a.last_used)}</td><td><div class="actions"><button class="small" data-probe="${esc(a.id)}">疎通確認</button><button class="small ghost" data-edit="${esc(a.id)}">管理</button></div></td></tr>`).join("") || '<tr><td colspan="7"><div class="empty">アカウントがありません。「登録」または「取り込む」から追加できます。</div></td></tr>'}</tbody></table></div><div class="pagination"><span>${accountPage.total}件中 ${accountPage.total ? filter.offset + 1 : 0}–${Math.min(accountPage.total, filter.offset + 50)}件</span><div class="actions"><button id="prev" ${filter.offset === 0 ? "disabled" : ""}>←</button><button id="next" ${filter.offset + 50 >= accountPage.total ? "disabled" : ""}>→</button></div></div></section>`;
    $("#add-account").onclick = addAccount;
    $("#import-accounts").onclick = act(async () => {
      const r = await api("/api/accounts/import", {
        method: "POST",
        body: "{}",
      });
      toast(`${r.imported}件を確認しました`);
      await refresh();
    });
    $("#search").onclick = () => {
      filter.q = $("#account-search").value;
      filter.status = $("#account-filter").value;
      filter.offset = 0;
      refresh().catch((e) => toast(e.message, true));
    };
    $("#account-search").onkeydown = (e) => {
      if (e.key === "Enter") $("#search").click();
    };
    $("#prev").onclick = () => {
      filter.offset -= 50;
      refresh();
    };
    $("#next").onclick = () => {
      filter.offset += 50;
      refresh();
    };
    $("#select-all").onchange = (e) =>
      el
        .querySelectorAll(".account-select")
        .forEach((x) => (x.checked = e.target.checked));
    for (const [id, enabled] of [
      ["bulk-enable", true],
      ["bulk-disable", false],
    ])
      $("#" + id).onclick = act(async () => {
        for (const input of el.querySelectorAll(".account-select:checked"))
          await api(`/api/accounts/${input.value}`, {
            method: "PATCH",
            body: JSON.stringify({ enabled }),
          });
        toast("選択したアカウントを更新しました");
        await refresh();
      });
    el.querySelectorAll("[data-probe]").forEach(
      (b) => (b.onclick = act(() => queueJob(b.dataset.probe, "probe"))),
    );
    el.querySelectorAll("[data-edit]").forEach(
      (b) => (b.onclick = act(() => editAccount(b.dataset.edit))),
    );
  }
  if (view === "jobs") {
    el.innerHTML =
      heading(
        "登録ジョブ",
        "メール確認・OAuth認証の進捗と、操作が必要な画面を確認できます。",
        `<button id="pause-creator">${overview.settings?.creator_paused ? "キューを再開" : "キューを一時停止"}</button><button class="primary" id="add-account">＋ アカウントを登録</button>`,
      ) +
      `<section class="panel"><div class="panel-head"><h2>最近のジョブ</h2><span class="muted">${overview.settings?.creator_paused ? "新しいジョブの実行を停止中" : "自動更新 · 1件ずつ実行"}</span></div><div class="table-wrap"><table><thead><tr><th>ジョブ</th><th>アカウント</th><th>状態</th><th>進捗</th><th>更新</th><th></th></tr></thead><tbody>${jobs.map((j) => `<tr><td><div class="name">${esc({ create: "アカウント登録", probe: "疎通確認", login: "再認証", retry: "登録を再試行" }[j.kind] || j.kind)}</div><div class="email">${esc(j.id.slice(0, 18))}</div></td><td>${esc(j.account_id)}</td><td>${badge(j.status)}</td><td>${esc(labels[j.stage] || j.stage)}</td><td class="muted">${time(j.updated_at)}</td><td><button class="small ${j.status === "waiting" ? "primary" : ""}" data-job="${esc(j.id)}">詳細・ブラウザー</button></td></tr>`).join("") || '<tr><td colspan="6"><div class="empty">登録ジョブはまだありません。</div></td></tr>'}</tbody></table></div></section>`;
    $("#add-account").onclick = addAccount;
    const jobPanel = el.querySelector(".panel");
    jobPanel
      .querySelector(".panel-head")
      .insertAdjacentHTML(
        "afterend",
        `<div class="toolbar"><select id="job-filter"><option value="">すべての状態</option>${["queued", "running", "waiting", "completed", "resolved", "needs_attention", "cancelled", "interrupted"].map((s) => `<option value="${s}" ${jobFilter.status === s ? "selected" : ""}>${labels[s] || s}</option>`).join("")}</select><span class="spacer"></span><button class="small danger" id="cancel-queued">待機ジョブをすべて停止</button></div>`,
      );
    jobPanel.insertAdjacentHTML(
      "beforeend",
      `<div class="pagination"><span>${jobPage.total}件中 ${jobPage.total ? jobFilter.offset + 1 : 0}–${Math.min(jobPage.total, jobFilter.offset + 50)}件</span><div class="actions"><button id="job-prev" ${jobFilter.offset === 0 ? "disabled" : ""}>←</button><button id="job-next" ${jobFilter.offset + 50 >= jobPage.total ? "disabled" : ""}>→</button></div></div>`,
    );
    $("#job-filter").onchange = () => {
      jobFilter.status = $("#job-filter").value;
      jobFilter.offset = 0;
      refresh();
    };
    $("#job-prev").onclick = () => {
      jobFilter.offset -= 50;
      refresh();
    };
    $("#job-next").onclick = () => {
      jobFilter.offset += 50;
      refresh();
    };
    $("#cancel-queued").onclick = act(async () => {
      const result = await api("/api/jobs/cancel-queued", {
        method: "POST",
        body: "{}",
      });
      toast(`${result.cancelled}件の待機ジョブを停止しました`);
      await refresh();
    });
    $("#pause-creator").onclick = act(async () => {
      const cfg = await api("/api/settings");
      cfg.pool.creator_paused = !cfg.pool.creator_paused;
      await api("/api/settings", {
        method: "PUT",
        body: JSON.stringify({ pool: cfg.pool }),
      });
      await refresh();
    });
    el.querySelectorAll("[data-job]").forEach(
      (b) => (b.onclick = () => openJob(b.dataset.job)),
    );
  }
  if (view === "keys") {
    el.innerHTML =
      heading(
        "APIキー",
        "クライアントごとにキーを発行し、不要になったキーを無効化できます。",
        '<button class="primary" id="new-key">＋ APIキーを作成</button>',
      ) +
      `<section class="panel"><div class="panel-head"><h2>接続先</h2></div><div class="panel-body"><div class="code-box">${esc(location.origin)}/v1</div><p class="sub">Chat Completions・Responses・SSEに対応。現在のAPIはテキスト入力用です。</p></div></section><section class="panel"><div class="table-wrap"><table><thead><tr><th>名前</th><th>キー</th><th>状態</th><th>作成</th><th>最終利用</th><th></th></tr></thead><tbody>${keys.map((k) => `<tr><td class="name">${esc(k.name)}</td><td><code>${esc(k.prefix)}…</code></td><td>${badge(k.enabled ? "ready" : "cancelled")}</td><td>${time(k.created_at)}</td><td>${time(k.last_used)}</td><td>${k.enabled ? `<button class="small danger" data-revoke="${esc(k.id)}">無効化</button>` : ""}</td></tr>`).join("") || '<tr><td colspan="6"><div class="empty">APIキーを作成してクライアントから接続できます。</div></td></tr>'}</tbody></table></div></section>`;
    $("#new-key").onclick = () =>
      modal(
        "APIキーを作成",
        '<div class="field"><label>名前</label><input id="key-name" placeholder="例：My chat app" required></div>',
        "作成",
        async () => {
          const value = await api("/api/keys", {
            method: "POST",
            body: JSON.stringify({ name: $("#key-name").value }),
          });
          closeModal();
          modal(
            "APIキーを保存してください",
            `<p class="screen-note">キーの全文はこの画面で一度だけ表示します。</p><div class="code-box" id="new-secret">${esc(value.token)}</div>`,
            "コピー",
            async () => {
              await navigator.clipboard.writeText(value.token);
              toast("コピーしました");
              closeModal();
              refresh();
            },
          );
        },
      );
    el.querySelectorAll("[data-revoke]").forEach(
      (b) =>
        (b.onclick = act(async () => {
          await api(`/api/keys/${b.dataset.revoke}`, { method: "DELETE" });
          await refresh();
        })),
    );
  }
  if (view === "play") {
    el.innerHTML =
      heading(
        "プレイグラウンド",
        "プール経由の生成とストリーミングをその場で確認。",
      ) +
      `<div class="grid-two"><section class="panel"><div class="panel-head"><h2>リクエスト</h2></div><div class="panel-body"><div class="field"><label>モデル</label><select id="play-model">${(overview.models || []).map((m) => `<option ${m === overview.settings?.default_model ? "selected" : ""}>${esc(m)}</option>`).join("")}</select></div><div class="field" style="margin-top:20px"><label>メッセージ</label><textarea id="play-prompt" rows="8" placeholder="メッセージを入力してください">日本語で短く自己紹介してください。</textarea></div><div class="settings-actions"><button class="primary" id="play-send">送信 →</button></div></div></section><section class="panel"><div class="panel-head"><h2>レスポンス</h2><span id="play-status" class="muted">Ready</span></div><div class="panel-body"><div class="chat-output" id="play-output">ここに応答が表示されます。</div></div></section></div>`;
    $("#play-send").onclick = act(async () => {
      const prompt = $("#play-prompt").value;
      const model = $("#play-model").value;
      if (!model) throw new Error("利用可能なモデルがありません");
      busy = true;
      $("#play-send").disabled = true;
      $("#play-output").textContent = "";
      $("#play-status").textContent = "Generating…";
      try {
        const response = await fetch("/v1/chat/completions", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            model,
            messages: [{ role: "user", content: prompt }],
            stream: true,
          }),
        });
        if (!response.ok)
          throw new Error((await response.json()).error.message);
        const reader = response.body.getReader(),
          decoder = new TextDecoder();
        let buffer = "";
        while (true) {
          const { value, done } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let boundary;
          while ((boundary = buffer.indexOf("\n\n")) >= 0) {
            const block = buffer.slice(0, boundary);
            buffer = buffer.slice(boundary + 2);
            for (const line of block.split("\n"))
              if (line.startsWith("data: ")) {
                const raw = line.slice(6);
                if (raw === "[DONE]") continue;
                const value = JSON.parse(raw);
                if (value.error) throw new Error(value.error.message);
                $("#play-output").textContent +=
                  value.choices?.[0]?.delta?.content || "";
              }
          }
        }
        $("#play-status").textContent = "Completed";
      } finally {
        busy = false;
        $("#play-send").disabled = false;
      }
    });
  }
  if (view === "settings") {
    const p = settings.pool,
      b = settings.email;
    el.innerHTML =
      heading("設定", "実行枠、待機時間、確認メールの接続先を管理。") +
      `<form id="settings-form"><section class="panel"><div class="panel-head"><h2>アカウントプール</h2></div><div class="panel-body form-grid">${[
        ["max_workers", "同時稼働プロセス", 1, 32],
        ["queue_limit", "待機リクエスト上限", 1, 4096],
        ["queue_seconds", "キュー待機時間（秒）", 1, 300],
        ["request_seconds", "生成タイムアウト（秒）", 10, 1800],
        ["idle_seconds", "未使用プロセスの停止（秒）", 10, 86400],
        ["lease_seconds", "アカウント入れ替えの目安（秒）", 10, 86400],
        ["lease_requests", "入れ替えまでのリクエスト数", 1, 10000],
      ]
        .map(
          ([id, label, min, max]) =>
            `<div class="field"><label>${label}</label><input name="${id}" type="number" min="${min}" max="${max}" value="${p[id]}" required></div>`,
        )
        .join(
          "",
        )}<div class="field"><label>既定モデル</label><input name="default_model" value="${esc(p.default_model)}"></div></div></section><section class="panel"><div class="panel-head"><h2>確認メール</h2>${badge(b.configured ? "ready" : "new")}</div><div class="panel-body form-grid"><div class="field full"><label>Email Worker URL</label><input name="base_url" type="url" value="${esc(b.base_url)}" required></div><div class="field"><label>受信ドメイン</label><input name="mail_domain" value="${esc(b.mail_domain)}" required></div><div class="field"><label>Worker APIトークン</label><input name="api_token" type="password" placeholder="変更するときだけ入力" autocomplete="new-password"><small>未入力の場合、現在のトークンを保持します。</small></div></div></section><div class="settings-actions"><button class="primary">変更を保存</button></div></form>`;
    $("#settings-form").onsubmit = act(async (event) => {
      event.preventDefault();
      const f = new FormData(event.target),
        pool = { ...p };
      for (const k of [
        "max_workers",
        "queue_limit",
        "queue_seconds",
        "request_seconds",
        "idle_seconds",
        "lease_seconds",
        "lease_requests",
      ])
        pool[k] = Number(f.get(k));
      pool.default_model = f.get("default_model");
      await api("/api/settings", {
        method: "PUT",
        body: JSON.stringify({
          pool,
          email: {
            base_url: f.get("base_url"),
            mail_domain: f.get("mail_domain"),
            api_token: f.get("api_token"),
          },
        }),
      });
      toast("設定を保存しました");
      await refresh();
    });
  }
  if (view === "egress") renderEgress();
}
const egressKinds = {
  direct: "直接接続",
  warp: "Docker WARP",
  vpn: "WireGuard VPN",
  proxy: "プロキシ",
  relay: "リレー",
};
const egressModes = {
  direct: "直接接続",
  sticky: "同じ出口を優先",
  spread: "出口を分散",
};
const egressModeSummary = { direct: "直接接続", sticky: "同じ出口を優先", spread: "出口を分散" };
function copyEgressConfig(config) {
  return {
    mode: config.mode || "direct",
    allow_direct_fallback: !!config.allow_direct_fallback,
    max_attempts: config.max_attempts || 2,
    cooldown_seconds: config.cooldown_seconds || 60,
    endpoints: (config.endpoints || []).map((endpoint) => ({ ...endpoint })),
  };
}
function egressURL(value) {
  try {
    const url = new URL(value);
    url.username = "";
    url.password = "";
    return url.toString();
  } catch {
    return "";
  }
}
function egressMarkDirty() {
  egressDirty = true;
  $("#egress-unsaved").hidden = false;
}
function egressReadForm() {
  const form = $("#egress-form");
  if (!form) return;
  const values = new FormData(form);
  egressDraft.mode = values.get("mode");
  egressDraft.allow_direct_fallback = values.has("allow_direct_fallback");
  egressDraft.max_attempts = Number(values.get("max_attempts"));
  egressDraft.cooldown_seconds = Number(values.get("cooldown_seconds"));
}
function renderEgress() {
  const config = egressDraft;
  $("#content").innerHTML =
    heading(
      "出口とIPの管理",
      "接続先を選び、利用制限や接続エラーが発生した出口を休止します。",
      '<button id="egress-refresh">更新</button>',
    ) +
    `<div id="egress-summary" class="cards"></div>
    <form id="egress-form">
      <section class="panel"><div class="panel-head"><h2>接続方法</h2><span id="egress-unsaved" class="badge waiting" ${egressDirty ? "" : "hidden"}>未保存の変更</span></div>
        <div class="panel-body form-grid">
          <div class="field full"><label for="egress-mode">出口の選び方</label><select id="egress-mode" name="mode">${Object.entries(egressModes).map(([id, label]) => `<option value="${id}" ${config.mode === id ? "selected" : ""}>${label}</option>`).join("")}</select><small>「同じ出口を優先」では、利用できる間は選んだ出口を使います。分散では利用可能な出口へ順番に割り振ります。</small></div>
          <div class="field"><label for="egress-attempts">再試行を含めた最大試行回数</label><input id="egress-attempts" name="max_attempts" type="number" min="1" max="4" value="${esc(config.max_attempts)}" required><small>応答が始まる前の失敗だけ、別の出口で再試行します。</small></div>
          <div class="field"><label for="egress-cooldown">出口を休止する時間（秒）</label><input id="egress-cooldown" name="cooldown_seconds" type="number" min="1" max="86400" value="${esc(config.cooldown_seconds)}" required><small>接続先が待機時間を指定した場合は、その時間を優先します。</small></div>
          <label class="switch-label field full"><input name="allow_direct_fallback" type="checkbox" ${config.allow_direct_fallback ? "checked" : ""}>出口が使えない場合は直接接続を許可</label>
        </div>
      </section>
      <section class="panel"><div class="panel-head"><h2>登録した出口</h2><button type="button" id="egress-add" class="small">＋ 出口を追加</button></div><div id="egress-endpoints"></div></section>
      <div class="settings-actions egress-settings-actions"><span class="muted">設定変更は保存後に適用されます。</span><button class="primary" id="egress-save">変更を保存</button></div>
    </form>
    <section class="panel"><div class="panel-head"><h2>現在の接続状態</h2><span class="muted">5秒ごとに更新</span></div><div id="egress-routes"></div><div class="panel-body egress-help"><p>出口の切り替えは実行中の応答が終わってから行います。Docker内のVPN・WARPを操作します。「VPNサーバーを切り替え」は次の候補へ移り、「同じサーバーに再接続」は接続先を変えずに再接続します。</p><p>VPNの接続状態はハンドシェイクと実際の出口IPで確認します。pingの応答だけではVPNの接続確認はできません。</p><p>「登録を更新して再接続」はWARPの登録を作り直します。再接続後の出口IPを測定しますが、同じIPになる場合があります。同じIPの利用制限は解除せず、休止時間を保持します。</p></div></section>
    <section class="panel"><div class="panel-head"><h2>最近の接続イベント</h2></div><div id="egress-events"></div></section>`;
  renderEgressEndpoints();
  renderEgressStatus();
  $("#egress-refresh").onclick = act(refresh);
  $("#egress-add").onclick = () => editEgressEndpoint();
  $("#egress-form").oninput = egressMarkDirty;
  $("#egress-form").onchange = egressMarkDirty;
  $("#egress-form").onsubmit = act(async (event) => {
    event.preventDefault();
    egressReadForm();
    const button = $("#egress-save");
    button.disabled = true;
    try {
      const config = {
        ...egressDraft,
        endpoints: egressDraft.endpoints.map(({ secret_configured, ...endpoint }) => endpoint),
      };
      await api("/api/egress", { method: "PUT", body: JSON.stringify(config) });
      egress = await api("/api/egress");
      egressDraft = copyEgressConfig(egress.config);
      egressDirty = false;
      renderEgress();
      toast("出口の設定を保存しました");
    } finally {
      if (button.isConnected) button.disabled = false;
    }
  });
}
function renderEgressEndpoints() {
  const endpoints = egressDraft.endpoints;
  $("#egress-endpoints").innerHTML = endpoints.length
    ? `<div class="table-wrap"><table><thead><tr><th>有効</th><th>出口</th><th>接続先</th><th>制限時の出口切り替え</th><th>操作</th></tr></thead><tbody>${endpoints.map((endpoint) => `<tr><td><input type="checkbox" data-egress-enabled="${esc(endpoint.id)}" aria-label="${esc(endpoint.name)}を有効化" ${endpoint.enabled ? "checked" : ""}></td><td><div class="name">${esc(endpoint.name || endpoint.id)}</div><div class="email">${esc(egressKinds[endpoint.kind] || endpoint.kind)}</div></td><td class="egress-url">${esc(egressURL(endpoint.url) || "—")}${endpoint.secret_configured || endpoint.secret ? '<div class="email">認証情報を設定済み</div>' : ""}</td><td>${["warp", "vpn"].includes(endpoint.kind) ? (endpoint.rotate_on_limit ? "自動で切り替え" : "休止のみ") : "—"}</td><td><div class="actions"><button type="button" class="small" data-egress-edit="${esc(endpoint.id)}">編集</button><button type="button" class="small danger" data-egress-delete="${esc(endpoint.id)}">削除</button></div></td></tr>`).join("")}</tbody></table></div>`
    : '<div class="empty">出口は未登録です。「出口を追加」からWARPやプロキシを登録できます。</div>';
  $("#egress-endpoints").querySelectorAll("[data-egress-enabled]").forEach((input) => {
    input.onchange = () => {
      egressDraft.endpoints.find((endpoint) => endpoint.id === input.dataset.egressEnabled).enabled = input.checked;
      egressMarkDirty();
    };
  });
  $("#egress-endpoints").querySelectorAll("[data-egress-edit]").forEach((button) => {
    button.onclick = () => editEgressEndpoint(button.dataset.egressEdit);
  });
  $("#egress-endpoints").querySelectorAll("[data-egress-delete]").forEach((button) => {
    button.onclick = () => {
      egressDraft.endpoints = egressDraft.endpoints.filter((endpoint) => endpoint.id !== button.dataset.egressDelete);
      egressMarkDirty();
      renderEgressEndpoints();
    };
  });
}
function editEgressEndpoint(id) {
  egressReadForm();
  const current = egressDraft.endpoints.find((endpoint) => endpoint.id === id);
  const endpoint = current || { name: "Docker WARP", kind: "warp", url: "", enabled: true, rotate_on_limit: true };
  modal(
    current ? "出口を編集" : "出口を追加",
    `<form id="egress-endpoint-form" class="form-grid"><div class="field"><label for="endpoint-name">表示名</label><input id="endpoint-name" value="${esc(endpoint.name)}" maxlength="100" required></div>
    <div class="field"><label for="endpoint-kind">種類</label><select id="endpoint-kind">${Object.entries(egressKinds).filter(([kind]) => kind !== "direct" || current?.kind === "direct").map(([kind, label]) => `<option value="${kind}" ${endpoint.kind === kind ? "selected" : ""}>${label}</option>`).join("")}</select></div>
    <div class="field full" id="endpoint-url-field"><label for="endpoint-url">接続URL</label><input id="endpoint-url" value="${esc(egressURL(endpoint.url))}" placeholder="http://127.0.0.1:18071" required><small id="endpoint-url-help"></small></div>
    <div class="field full" id="endpoint-secret-field"><label for="endpoint-secret" id="endpoint-secret-label">認証トークン</label><input id="endpoint-secret" type="password" autocomplete="new-password" placeholder="${endpoint.secret_configured || endpoint.secret ? "設定済み・変更するときだけ入力" : "必要な場合だけ入力"}"><small id="endpoint-secret-help">未入力の場合、現在のトークンを保持します。</small></div>
    <div class="field full" id="endpoint-rotate-field"><label class="switch-label"><input id="endpoint-rotate" type="checkbox" ${endpoint.rotate_on_limit ? "checked" : ""}><span id="endpoint-rotate-label">利用制限時にDocker WARPのIPを自動で切り替え</span></label><small>再接続しても同じIPになる場合があります。その場合は休止時間を保持します。</small></div>
    <label class="switch-label field full"><input id="endpoint-enabled" type="checkbox" ${endpoint.enabled ? "checked" : ""}>この出口を有効にする</label></form>`,
    "変更を反映",
    async () => {
      if (!$("#egress-endpoint-form").reportValidity()) return;
      const kind = $("#endpoint-kind").value;
      const updated = {
        id: current?.id || `egress_${crypto.randomUUID().replaceAll("-", "").slice(0, 16)}`,
        name: $("#endpoint-name").value.trim(),
        kind,
        url: kind === "direct" ? "" : current && kind === current.kind && $("#endpoint-url").value.trim() === egressURL(current.url) ? current.url : $("#endpoint-url").value.trim(),
        enabled: $("#endpoint-enabled").checked,
        rotate_on_limit: ["warp", "vpn"].includes(kind) && $("#endpoint-rotate").checked,
        secret_configured: !!endpoint.secret_configured,
      };
      const secret = $("#endpoint-secret").value;
      if (secret) updated.secret = secret;
      else if (endpoint.secret) updated.secret = endpoint.secret;
      if (current) egressDraft.endpoints.splice(egressDraft.endpoints.indexOf(current), 1, updated);
      else egressDraft.endpoints.push(updated);
      closeModal();
      egressMarkDirty();
      renderEgressEndpoints();
    },
  );
  function updateKind() {
    const kind = $("#endpoint-kind").value;
    $("#endpoint-url-field").hidden = kind === "direct";
    $("#endpoint-url").required = kind !== "direct";
    $("#endpoint-secret-field").hidden = kind === "direct";
    $("#endpoint-secret-label").textContent = kind === "proxy" ? "プロキシ認証（ユーザー名:パスワード）" : "認証トークン";
    $("#endpoint-secret-help").textContent = kind === "proxy" ? "例：user:password。未入力の場合、現在の認証情報を保持します。" : "未入力の場合、現在のトークンを保持します。";
    $("#endpoint-rotate-field").hidden = !["warp", "vpn"].includes(kind);
    $("#endpoint-rotate-label").textContent = kind === "vpn" ? "利用制限時に次のVPNサーバーへ自動で切り替え" : "利用制限時にDocker WARPのIPを自動で切り替え";
    $("#endpoint-url").placeholder = kind === "vpn" ? "http://127.0.0.1:18101" : kind === "proxy" ? "http://127.0.0.1:1080" : "http://127.0.0.1:18071";
    $("#endpoint-url-help").textContent = kind === "proxy"
      ? "HTTP・HTTPS・SOCKS5プロキシのURL。認証情報を含む既存URLは、そのまま保存すると保持されます。"
      : kind === "vpn" ? "WireGuard VPNを管理するリレーの接続URLを入力します。" : "Docker WARPまたはリレーの接続URLを入力します。";
  }
  $("#endpoint-kind").onchange = updateKind;
  $("#egress-endpoint-form").onsubmit = (event) => { event.preventDefault(); $("#modal-submit").click(); };
  updateKind();
}
function egressCooldown(until) {
  const remaining = Math.ceil(Number(until) - Date.now() / 1000);
  if (!(remaining > 0)) return "—";
  const duration = remaining >= 60 ? `${Math.ceil(remaining / 60)}分` : `${remaining}秒`;
  return `${duration}後に再開（${time(Number(until))}）`;
}
function egressRouteEnabled(route) {
  if (route.enabled === false || egress.config.endpoints?.find((endpoint) => endpoint.id === route.id)?.enabled === false) return false;
  return route.kind === "direct" ? egress.config.mode === "direct" || egress.config.allow_direct_fallback : egress.config.mode !== "direct";
}
function egressRouteCooldowns(route) {
  if (Array.isArray(route.cooldowns)) return route.cooldowns.filter((limit) => limit.until > Date.now() / 1000);
  return route.cooldown_until > Date.now() / 1000 ? [{ model: "", until: route.cooldown_until }] : [];
}
function egressHandshakeAge(value) {
  const seconds = Math.floor(Date.now() / 1000 - Number(value));
  if (!Number.isFinite(seconds) || !(Number(value) > 0)) return "未確認";
  if (seconds < 60) return `${Math.max(0, seconds)}秒前`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}分前`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}時間前`;
  return `${Math.floor(seconds / 86400)}日前`;
}
function egressVPNDetails(route) {
  if (route.kind !== "vpn" && route.backend !== "wireguard") return "";
  if (route.pooled) return `<div class="egress-vpn-details">WireGuard ${Number(route.pool_size) || 0}接続のプール<div>候補サーバー ${Number(route.candidate_count) || 0}件</div></div>`;
  const server = route.vpn_server || route.server_hostname || "";
  const endpoint = route.vpn_endpoint || "";
  return `<div class="egress-vpn-details">${server ? `<div>サーバー: ${esc(server)}</div>` : ""}${endpoint ? `<div>接続先: ${esc(endpoint)}</div>` : ""}<div title="${esc(time(Number(route.latest_handshake)))}">ハンドシェイク: ${esc(egressHandshakeAge(route.latest_handshake))}</div></div>`;
}
function egressPoolStatus(routes) {
  let container = $("#egress-pools");
  if (!container) {
    container = document.createElement("div");
    container.id = "egress-pools";
    $("#egress-routes").after(container);
  }
  container.innerHTML = routes.filter((route) => route.pooled).map((route) => {
    const slots = route.slots || [];
    return `<section class="egress-pool-block" data-egress-pool="${esc(route.id)}"><div class="egress-pool-head"><div><h3>${esc(route.name || route.id)} · VPN接続の内訳</h3><p class="sub">利用可能 ${Number(route.ready_count) || 0} / ${Number(route.pool_size) || 0}接続 · 休止 ${Number(route.cooling_count) || 0}接続 · 候補 ${Number(route.candidate_count) || 0}サーバー</p></div><span class="badge ${route.healthy ? "ready" : "waiting"}">${route.healthy ? "利用可能な接続あり" : "利用できる接続なし"}</span></div><div class="table-wrap"><table><thead><tr><th>接続</th><th>サーバー</th><th>実際の公開IP</th><th>状態</th><th>通信中</th><th>リクエスト</th><th>休止時間</th></tr></thead><tbody>${slots.map((slot) => {
      const cooling = slot.cooldown_until > Date.now() / 1000;
      const status = slot.rotating ? "接続切り替え中" : cooling ? "休止中" : slot.healthy && slot.connected ? "利用可能" : "未接続 / 接続失敗";
      const style = slot.rotating || cooling ? "waiting" : slot.healthy && slot.connected ? "ready" : "failed";
      return `<tr data-vpn-slot="${esc(slot.id)}"><td><div class="name">${esc(slot.id)}</div><div class="email">ハンドシェイク: ${esc(egressHandshakeAge(slot.latest_handshake))}</div>${slot.last_error ? `<div class="error-text egress-last-error">${esc(slot.last_error)}</div>` : ""}</td><td><div class="name">${esc(slot.vpn_server || "未確認")}</div><div class="email">${esc(slot.vpn_endpoint || "—")}</div></td><td><code>${esc(slot.public_ip || "未確認")}</code>${slot.ip_scope ? `<div class="email">確認先: ${esc(slot.ip_scope)}</div>` : ""}</td><td><span class="badge ${style}">${status}</span></td><td class="egress-slot-active">${Number(slot.active_flows) || 0}</td><td>${Number(slot.requests) || 0}<div class="email">成功 ${Number(slot.successes) || 0} / 失敗 ${Number(slot.failures) || 0}</div></td><td class="muted">${esc(egressCooldown(slot.cooldown_until))}</td></tr>`;
    }).join("") || '<tr><td colspan="7"><div class="empty">VPN接続の状態は、疎通確認後に表示されます。</div></td></tr>'}</tbody></table></div></section>`;
  }).join("");
  container.querySelectorAll(".egress-pool-head > div").forEach((head) => {
    const note = document.createElement("p");
    note.className = "sub";
    note.textContent = "処理中の接続は維持し、空いている接続だけを切り替えます。すべて処理中の場合、手動再接続は開始しません。";
    head.append(note);
  });
}
function renderEgressStatus() {
  if (!$("#egress-routes")) return;
  const routes = egress.routes || [];
  const active = routes.reduce((sum, route) => sum + (route.pooled ? (route.slots || []).reduce((flows, slot) => flows + (Number(slot.active_flows) || 0), 0) : Number(route.active) || 0), 0);
  const selected = routes.filter(egressRouteEnabled);
  const capacity = selected.reduce((sum, route) => sum + (route.pooled ? Number(route.pool_size) || 0 : 1), 0);
  const available = selected.filter((route) => route.healthy && !route.rotating && !egressRouteCooldowns(route).some((limit) => !limit.model)).reduce((sum, route) => sum + (route.pooled ? Number(route.ready_count) || 0 : 1), 0);
  const cooldown = routes.reduce((sum, route) => sum + (route.pooled ? Math.max(Number(route.cooling_count) || 0, egressRouteCooldowns(route).length ? 1 : 0) : egressRouteCooldowns(route).length ? 1 : 0), 0);
  $("#egress-summary").innerHTML = `<div class="card"><div class="card-label">利用可能な接続 ${icon("egress")}</div><div class="card-value">${available}<small> / ${capacity}</small></div><div class="card-foot">現在の接続設定で利用</div></div><div class="card"><div class="card-label">処理中 ${icon("activity")}</div><div class="card-value">${active}</div><div class="card-foot">通信中のリクエスト</div></div><div class="card"><div class="card-label">利用制限のある接続 ${icon("jobs")}</div><div class="card-value">${cooldown}</div><div class="card-foot">全体または一部モデルを休止</div></div><div class="card"><div class="card-label">現在の接続方法 ${icon("settings")}</div><div class="egress-mode-value">${esc(egressModeSummary[egress.config.mode] || egress.config.mode)}</div><div class="card-foot">保存済みの設定</div></div>`;
  $("#egress-routes").innerHTML = `<div class="table-wrap"><table><thead><tr><th>出口</th><th>実際の公開IP</th><th>状態</th><th>処理中</th><th>リクエスト</th><th>休止時間</th><th>操作</th></tr></thead><tbody>${routes.map((route) => {
    const disabled = egress.config.endpoints?.find((endpoint) => endpoint.id === route.id)?.enabled === false;
    const limits = egressRouteCooldowns(route);
    const wholeCooldown = limits.some((limit) => !limit.model);
    const status = disabled ? "無効" : !egressRouteEnabled(route) ? "対象外" : route.rotating ? "IP切り替え待ち" : wholeCooldown ? "休止中" : limits.length ? "一部モデル休止" : route.healthy ? "利用可能" : "未確認 / 接続失敗";
    const style = disabled || !egressRouteEnabled(route) ? "queued" : route.rotating || limits.length ? "waiting" : route.healthy ? "ready" : "new";
    const cooldownDetail = limits.length ? limits.map((limit) => `<div class="egress-cooldown-detail"><div class="email">${esc(limit.model || "すべてのモデル")}</div>${esc(egressCooldown(limit.until))}</div>`).join("") : "—";
    const pending = egressActions.has(route.id);
    return `<tr><td><div class="name">${esc(route.name || route.id)}</div><div class="email">${esc(egressKinds[route.kind] || route.kind)}${route.rotations ? ` · 出口切り替え ${Number(route.rotations)}回` : ""}</div>${egressVPNDetails(route)}${route.last_error ? `<div class="error-text egress-last-error">${esc(route.last_error)}</div>` : ""}</td><td><code>${esc(route.public_ip || "未確認")}</code>${route.ip_scope ? `<div class="email egress-ip-scope">確認先: ${esc(route.ip_scope)}</div>` : ""}</td><td><span class="badge ${style}">${status}</span></td><td>${Number(route.active) || 0}</td><td>${Number(route.requests) || 0}<div class="email">成功 ${Number(route.successes) || 0} / 失敗 ${Number(route.failures) || 0}</div></td><td class="muted">${cooldownDetail}</td><td><div class="actions egress-route-actions"><button class="small" data-egress-probe="${esc(route.id)}" ${pending || route.rotating ? "disabled" : ""}>${pending ? "処理中…" : "疎通確認"}</button>${["warp", "vpn"].includes(route.kind) ? `<button class="small" data-egress-rotate="${esc(route.id)}" ${pending || route.rotating ? "disabled" : ""}>${route.kind === "vpn" ? "VPNサーバーを切り替え" : "IPを切り替え"}</button>` : ""}${route.kind === "vpn" ? `<button class="small" data-egress-reconnect="${esc(route.id)}" ${pending || route.rotating ? "disabled" : ""}>同じサーバーに再接続</button>` : ""}${route.kind === "warp" ? `<button class="small" data-egress-renew="${esc(route.id)}" ${pending || route.rotating ? "disabled" : ""}>登録を更新して再接続</button>` : ""}</div></td></tr>`;
  }).join("") || '<tr><td colspan="7"><div class="empty">利用できる出口がありません。出口を追加するか、接続方法を変更してください。</div></td></tr>'}</tbody></table></div>`;
  $("#egress-routes").querySelectorAll("tbody > tr").forEach((row, index) => {
    const route = routes[index];
    if (!route?.pooled) return;
    row.children[1].innerHTML = '<strong>複数の出口IP</strong><div class="email">接続ごとのIPは下の内訳に表示</div>';
    row.children[2].insertAdjacentHTML("beforeend", `<div class="email">利用可能 ${Number(route.ready_count) || 0} / ${Number(route.pool_size) || 0}接続</div>`);
    row.children[3].textContent = (route.slots || []).reduce((sum, slot) => sum + (Number(slot.active_flows) || 0), 0);
  });
  egressPoolStatus(routes);
  for (const action of ["probe", "rotate", "renew", "reconnect"]) {
    $("#egress-routes").querySelectorAll(`[data-egress-${action}]`).forEach((button) => {
      button.onclick = act(async () => {
        const id = button.dataset[`egress${action[0].toUpperCase()}${action.slice(1)}`];
        egressActions.add(id);
        renderEgressStatus();
        try {
          const body = action === "renew" ? { renew_registration: true, max_attempts: 1 } : action === "reconnect" ? { same_server: true, max_attempts: 1 } : {};
          const result = await api(`/api/egress/${encodeURIComponent(id)}/${["renew", "reconnect"].includes(action) ? "rotate" : action}`, { method: "POST", body: JSON.stringify(body) });
          if (action === "probe") {
            toast(result?.healthy === false ? result.last_error || "出口へ接続できませんでした" : result?.pooled ? `疎通確認が完了しました（利用可能 ${Number(result.ready_count) || 0} / ${Number(result.pool_size) || 0}接続）` : `疎通確認が完了しました${result?.public_ip ? `（${result.public_ip}）` : ""}`, result?.healthy === false);
          } else if (result?.healthy === false) {
            toast(result.last_error || result.error?.message || "再接続後の出口を利用できません。接続状態を確認してください。", true);
          } else if (result?.changed === false) {
            toast("再接続しましたが、公開IPは変わりませんでした。休止時間を保持します。");
          } else {
            toast(`IPの切り替えが完了しました${result?.new_ip || result?.public_ip ? `（${result.new_ip || result.public_ip}）` : ""}`);
          }
        } catch (error) {
          if (error.status === 409) toast(`再接続を開始できませんでした。${error.message}。処理中の通信がある場合は、完了してから再試行してください。`, true);
          else throw error;
        } finally {
          egressActions.delete(id);
          await refresh();
        }
      });
    });
  }
  const events = egress.events || [];
  $("#egress-events").innerHTML = events.length
    ? `<div class="table-wrap"><table><thead><tr><th>日時</th><th>出口 / IP</th><th>モデル</th><th>結果</th><th>内容</th></tr></thead><tbody>${events.slice(0, 50).map((event) => `<tr><td class="muted">${time(event.created_at)}</td><td>${esc(routes.find((route) => route.id === event.endpoint_id)?.name || event.endpoint_id || "—")}<div class="email">${esc(event.public_ip || "—")}</div></td><td>${esc(event.model || "—")}</td><td>${esc(event.status || event.kind || "—")}</td><td class="egress-event-message">${esc(event.message || "—")}</td></tr>`).join("")}</tbody></table></div>`
    : '<div class="empty">接続イベントはまだありません。</div>';
}
function modal(title, body, action, onSubmit, wide = false) {
  closeModal();
  const wrap = document.createElement("div");
  wrap.className = "modal-backdrop";
  wrap.innerHTML = `<section class="modal ${wide ? "wide" : ""}"><div class="modal-header"><h2>${esc(title)}</h2><button id="close-modal" aria-label="閉じる">×</button></div><div class="modal-content">${body}</div>${action ? `<div class="modal-footer"><button id="modal-cancel">キャンセル</button><button class="primary" id="modal-submit">${esc(action)}</button></div>` : ""}</section>`;
  document.body.append(wrap);
  $("#close-modal").onclick = closeModal;
  if (action) {
    $("#modal-cancel").onclick = closeModal;
    $("#modal-submit").onclick = act(async () => {
      const b = $("#modal-submit");
      b.disabled = true;
      try {
        await onSubmit();
      } finally {
        if (b.isConnected) b.disabled = false;
      }
    });
  }
}
function closeModal() {
  clearInterval(modalTimer);
  clearInterval(browserTimer);
  $(".modal-backdrop")?.remove();
  if (frameURL) {
    URL.revokeObjectURL(frameURL);
    frameURL = undefined;
  }
}
function addAccount() {
  modal(
    "アカウントを登録",
    `<p class="screen-note">登録からメール確認、OAuth認証、モデルの疎通確認まで順番に実行します。</p><div class="form-grid"><div class="field"><label>表示名・接頭辞</label><input id="add-name" value="Account"></div><div class="field"><label>作成件数</label><input id="add-count" type="number" min="1" max="1000" value="1"></div><div class="field full"><label>メールアドレス（任意・1件の場合）</label><input id="add-email" type="email" placeholder="未入力なら受信ドメイン内のアドレスを自動生成"></div><div class="field full"><label>タグ</label><input id="add-tags" placeholder="カンマ区切り、例：main, free"></div></div><div class="note" style="margin-top:22px">処理は1件ずつ進みます。確認操作が必要になったら、ジョブのブラウザー画面から操作できます。</div>`,
    "登録を開始",
    async () => {
      const result = await api("/api/accounts", {
        method: "POST",
        body: JSON.stringify({
          count: Number($("#add-count").value),
          name: $("#add-name").value,
          email: $("#add-email").value,
          tags: $("#add-tags")
            .value.split(",")
            .map((s) => s.trim())
            .filter(Boolean),
        }),
      });
      closeModal();
      toast(`${result.length}件をキューに追加しました`);
      await navigate("jobs");
      if (result.length === 1) openJob(result[0].id);
    },
  );
}
async function queueJob(id, kind) {
  const job = await api(`/api/accounts/${id}/jobs`, {
    method: "POST",
    body: JSON.stringify({ kind }),
  });
  toast("ジョブを追加しました");
  openJob(job.id);
}
async function editAccount(id) {
  const a = await api(`/api/accounts/${id}`);
  modal(
    "アカウントを管理",
    `<div class="form-grid"><div class="field full"><label>表示名</label><input id="edit-name" value="${esc(a.name)}"></div><div class="field full"><label>タグ</label><input id="edit-tags" value="${esc(a.tags.join(", "))}"></div><label class="switch-label"><input id="edit-enabled" type="checkbox" ${a.enabled ? "checked" : ""}>プールで利用する</label></div><p class="sub">${esc(a.email)}</p>${a.last_error ? `<div class="note" style="margin-top:18px">${esc(a.last_error)}</div>` : ""}<div class="actions" style="margin-top:24px"><button id="reauth">再認証</button><button id="reset-cooldown">休止状態を解除</button><button class="danger" id="archive">一覧から削除</button></div><p class="screen-note" style="margin-top:15px">一覧から削除しても、保存済みのプロファイルは保持されます。</p>`,
    "保存",
    async () => {
      await api(`/api/accounts/${id}`, {
        method: "PATCH",
        body: JSON.stringify({
          name: $("#edit-name").value,
          tags: $("#edit-tags")
            .value.split(",")
            .map((s) => s.trim())
            .filter(Boolean),
          enabled: $("#edit-enabled").checked,
        }),
      });
      closeModal();
      await refresh();
    },
  );
  $("#reauth").onclick = act(() => queueJob(id, "login"));
  $("#reauth").insertAdjacentHTML(
    "afterend",
    '<button id="open-browser">ブラウザーを開く</button>',
  );
  $("#open-browser").onclick = act(() => queueJob(id, "browser"));
  $("#reset-cooldown").onclick = act(async () => {
    await api(`/api/accounts/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ reset_cooldown: true }),
    });
    toast("休止状態を解除しました");
  });
  $("#archive").onclick = act(async () => {
    await api(`/api/accounts/${id}`, { method: "DELETE" });
    closeModal();
    await refresh();
  });
}
async function openJob(id) {
  modal(
    "登録ジョブ",
    `<div class="actions" style="justify-content:space-between;margin-bottom:15px"><div id="job-status">読み込み中…</div><div class="actions"><button id="resume-job" class="small">再開</button><button id="cancel-job" class="small danger">停止</button></div></div><p class="screen-note">確認操作が必要な場合は、下のブラウザー画面をクリックして操作してください。キーボード・スクロールにも対応しています。</p><div class="browser-screen" id="screen" tabindex="0"><img id="browser-frame" alt="登録処理のブラウザー画面" hidden><div class="empty" id="frame-empty">ブラウザーが起動すると画面が表示されます。</div></div><div class="browser-controls"><button data-key="Tab" class="small">Tab</button><button data-key="Enter" class="small">Enter</button><button data-key="Backspace" class="small">⌫</button><input id="remote-text" placeholder="ブラウザーへ入力する文字"><button id="send-text" class="small">入力</button></div><div class="logs" id="job-logs"></div>`,
    null,
    null,
    true,
  );
  const command = (value) =>
    api(`/api/jobs/${id}/input`, {
      method: "POST",
      body: JSON.stringify(value),
    });
  $("#job-status").parentElement.insertAdjacentHTML(
    "afterend",
    '<div id="job-resolution" class="note" hidden style="margin-bottom:15px"></div>',
  );
  $("#screen").onclick = act(async (e) => {
    const img = $("#browser-frame");
    if (e.target !== img) return;
    const box = img.getBoundingClientRect();
    $("#screen").focus();
    await command({
      type: "click",
      x: ((e.clientX - box.left) / box.width) * frameSize.width,
      y: ((e.clientY - box.top) / box.height) * frameSize.height,
    });
  });
  $("#screen").onwheel = (e) => {
    e.preventDefault();
    command({ type: "scroll", delta_x: e.deltaX, delta_y: e.deltaY }).catch(
      () => {},
    );
  };
  $("#screen").onkeydown = (e) => {
    if (e.key.length === 1 && !e.ctrlKey && !e.altKey) {
      e.preventDefault();
      command({ type: "text", text: e.key }).catch(() => {});
      return;
    }
    const allowed = [
      "Tab",
      "Enter",
      "Backspace",
      "Delete",
      "Escape",
      "ArrowLeft",
      "ArrowRight",
      "ArrowUp",
      "ArrowDown",
      "Home",
      "End",
      "PageUp",
      "PageDown",
    ];
    if (allowed.includes(e.key) || (e.ctrlKey && e.key === "a")) {
      e.preventDefault();
      command({
        type: "key",
        key: (e.ctrlKey ? "Control+" : "") + e.key,
      }).catch(() => {});
    }
  };
  document
    .querySelectorAll("[data-key]")
    .forEach(
      (b) =>
        (b.onclick = act(() => command({ type: "key", key: b.dataset.key }))),
    );
  $("#send-text").onclick = act(async () => {
    await command({ type: "text", text: $("#remote-text").value });
    $("#remote-text").value = "";
  });
  $("#cancel-job").onclick = act(async () => {
    await api(`/api/jobs/${id}/cancel`, { method: "POST", body: "{}" });
    toast("停止を要求しました");
  });
  $("#resume-job").onclick = act(async () => {
    const result = await api(`/api/jobs/${id}/resume`, {
      method: "POST",
      body: JSON.stringify({ retry_signup: false }),
    });
    openJob(result.id);
  });
  $("#resume-job").insertAdjacentHTML(
    "afterend",
    '<button class="small" id="retry-signup" hidden>登録送信を再試行</button>',
  );
  $("#retry-signup").onclick = act(async () => {
    const result = await api(`/api/jobs/${id}/resume`, {
      method: "POST",
      body: JSON.stringify({ retry_signup: true }),
    });
    openJob(result.id);
  });
  let fetchingFrame = false;
  let browserActive = false;
  async function loadFrame() {
    if (!browserActive || fetchingFrame || !$("#browser-frame")) return;
    fetchingFrame = true;
    try {
      const r = await fetch(`/api/jobs/${id}/frame`);
      if (r.status !== 200) return;
      const blob = await r.blob();
      const width = Number(r.headers.get("X-Viewport-Width")),
        height = Number(r.headers.get("X-Viewport-Height"));
      if (width && height) frameSize = { width, height };
      const old = frameURL;
      frameURL = URL.createObjectURL(blob);
      const img = $("#browser-frame");
      if (img) {
        img.src = frameURL;
        img.hidden = false;
        $("#frame-empty").hidden = true;
      }
      if (old) URL.revokeObjectURL(old);
    } finally {
      fetchingFrame = false;
    }
  }
  async function loadDetails() {
    try {
      const data = await api(`/api/jobs/${id}`);
      if (!$("#job-status")) return;
      $("#job-status").innerHTML =
        `${badge(data.job.status)} &nbsp; ${esc(labels[data.job.stage] || data.job.stage)}`;
      const finished = ["completed", "resolved"].includes(data.job.status);
      browserActive = ["running", "waiting"].includes(data.job.status);
      $("#screen").hidden = finished;
      $(".modal-content .screen-note").hidden = finished;
      $(".modal-content .browser-controls").hidden = finished;
      const resolution = $("#job-resolution");
      resolution.hidden = !data.job.resolved_by;
      if (data.job.resolved_by) {
        resolution.innerHTML = `再開先のジョブで正常に完了しています。 <button class="small" id="open-resolved-job">完了したジョブを見る</button>`;
        $("#open-resolved-job").onclick = () => openJob(data.job.resolved_by);
      }
      $("#job-logs").innerHTML =
        data.events
          .map(
            (e) =>
              `<div class="log-line"><time>${new Date(e.created_at * 1000).toLocaleTimeString("ja-JP")}</time><span>${esc(labels[e.stage] || e.stage)}${e.message ? " · " + esc(e.message) : ""}</span></div>`,
          )
          .join("") +
        (data.job.error
          ? `<div class="error-text">${esc(data.job.error)}</div>`
          : "");
      $("#resume-job").disabled = [
        "running",
        "waiting",
        "queued",
        "completed",
        "resolved",
      ].includes(data.job.status);
      $("#retry-signup").hidden =
        ["running", "waiting", "queued", "completed", "resolved"].includes(
          data.job.status,
        ) ||
        !["new", "signup_submitting", "signup_action_required"].includes(
          data.job.stage,
        );
      document
        .querySelectorAll(".browser-controls input,.browser-controls button")
        .forEach(
          (el) =>
            (el.disabled = !["running", "waiting"].includes(data.job.status)),
        );
      $("#cancel-job").disabled = !["running", "waiting", "queued"].includes(
        data.job.status,
      );
    } catch (e) {
      toast(e.message, true);
    }
  }
  await loadDetails();
  loadFrame();
  modalTimer = setInterval(loadDetails, 2500);
  browserTimer = setInterval(loadFrame, 1200);
}
async function boot() {
  try {
    overview = await api("/api/overview");
    shell();
    await refresh();
  } catch (e) {
    if (!$("#login-form")) showLogin(e.message);
  }
}
const access = new URLSearchParams(location.hash.slice(1)).get("access");
if (access) {
  history.replaceState(null, "", location.pathname);
  try {
    await api("/api/login", {
      method: "POST",
      body: JSON.stringify({ key: access }),
    });
  } catch (e) {
    showLogin(e.message);
  }
}
await boot();
setInterval(() => {
  if (
    !$("#content") ||
    $(".modal-backdrop") ||
    busy ||
    ["settings", "play"].includes(view) ||
    (view !== "egress" && document.activeElement?.matches("input,textarea,select"))
  )
    return;
  refresh().catch(() => {});
}, 5000);
