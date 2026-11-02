// Gemini: Antigravity's pools, opened on a schedule. A window's clock starts
// at the first call that draws on it, so a pool nothing has called never
// begins its hours; the watching reads every pool of every account upstream
// and makes that one call for each pool while nothing is used, with a model
// of that pool. "Gemini" and "Claude & GPT" have a 5-hour window and a 7-day
// one; some plans' "Gemini 4 Argon" has the weekly one alone.
//
// A page the Gemini plugin adds to the window (kernel.js), its words in
// i18n.js beside this file. Classic script, loaded after app.js: it
// registers the page and touches nothing else. mount, at magpie.start, is
// handed the page; open reads it, and the page keeps itself while shown.
magpie.plugin("gemini", (ctx) => {
  let state = null; // /api/gemini/custom
  let picked = ""; // the account the usage view below is narrowed to
  let win = "5h"; // which window it is narrowed to: "5h" or "7d"
  let failed = "";
  let loading = null;
  let timer = 0;

  let main = null; // the page's own element, handed to mount
  const root = () => main;
  const shown = () => root() && !root().hidden;

  // periodOf is a window's length as the usage ledger names one: Go's own
  // duration grammar, which has no "7d" — a week is 168 hours.
  const periodOf = (w) => (w === "7d" ? "last:168h" : "last:5h");

  // fmtEvery says the longest wait as the reader set it.
  const fmtEvery = (sec) => {
    if (!sec) return t("magpie's own");
    if (sec % 3600 === 0) return t("{n} h", { n: sec / 3600 });
    return t("{n} min", { n: Math.round(sec / 60) });
  };

  // used reports whether anything has drawn on a window yet: the clock
  // that starts at the first call runs only once something has.
  const used = (w) => !!w && !w.unlimited && (w.used || 0) > 0;

  // asked is the Refresh button: the server reads the allowances now instead
  // of answering with the look it made minutes ago, which can be a snapshot
  // from before a window reset.
  async function read(force, asked) {
    if (loading && !force) return loading;
    loading = api(asked ? "gemini/custom?asked=1" : "gemini/custom")
      .then((s) => { state = s; failed = ""; })
      .catch((e) => { failed = e.message; })
      .finally(() => { loading = null; draw(); });
    return loading;
  }

  // save writes the mode and the interval back, and draws what came of it.
  async function save(patch) {
    const s = state || {};
    try {
      state = await api("gemini/custom", { on: patch.on ?? s.on, every: patch.every ?? s.every });
      failed = "";
      draw();
    } catch (e) {
      failed = e.message;
      draw();
    }
  }

  // ---- drawing ---------------------------------------------------------

  function draw() {
    const el0 = root();
    if (!el0) return;
    el0.replaceChildren();
    if (!state) {
      el0.append(el("div", "list", failed || t("Reading…")));
      return;
    }
    el0.append(head(), modeCard(), accounts(), records());
  }

  function head() {
    const h = el("div", "row-head");
    h.append(el("span", "label", t("Gemini")));
    h.append(el("span", "note", t("Antigravity accounts, opened on a schedule")));
    h.append(el("span", "grow"));
    const again = el("button", "text", t("Refresh"));
    again.type = "button";
    again.onclick = () => { again.classList.add("busy"); read(true, true).finally(() => again.classList.remove("busy")); };
    h.append(again);
    return h;
  }

  // modeCard is the setting: on, and the longest wait between two looks.
  function modeCard() {
    const card = el("div", "gm-card");
    const top = el("div", "gm-top");
    const sw = el("button", "gm-switch" + (state.on ? " on" : ""), state.on ? t("On") : t("Off"));
    sw.type = "button";
    sw.setAttribute("aria-pressed", String(!!state.on));
    sw.onclick = () => save({ on: !state.on });
    top.append(el("div", "gm-title", t("5 hours") + " · " + t("7 days")), sw);
    card.append(top);
    card.append(el("div", "gm-sub", t("A window's clock starts at the first call that draws on it: every pool an account has is opened once, each with a model of its own, and one already in use is looked at again after a wait inside the interval below.")));

    const row = el("div", "gm-row");
    row.append(el("span", "gm-k", t("Longest wait between looks")));
    const box = el("div", "segs");
    for (const sec of [0, 300, 600, 1800, 3600]) {
      const b = el("button", "opt" + ((state.every || 0) === sec ? " on" : ""), fmtEvery(sec));
      b.type = "button";
      b.onclick = () => save({ every: sec });
      box.append(b);
    }
    row.append(box);
    row.append(el("span", "gm-note", t("The wait is random, from {lo} to {hi}, so a first call never lands on the same second twice.", { lo: fmtEvery(Math.round((state.every || state.default) * 0.8)), hi: fmtEvery(state.every || state.default) })));
    card.append(row);

    if (failed) card.append(el("div", "gm-err", failed));
    return card;
  }

  // accounts draws one card an account: every pool it has, and what the
  // watching has done about each.
  function accounts() {
    const rows = state.rows || [];
    if (!rows.length) {
      return el("div", "gm-empty", t("No Antigravity account is signed in. Add one on the Providers page, and its windows show up here."));
    }
    const wrap = el("div", "gm-accounts");
    for (const r of rows) wrap.append(accountCard(r));
    return wrap;
  }

  // govOf is the window whose own figure says whether a pool's clock is
  // running: the 5-hour one where the pool has one, else the 7-day one,
  // which is the only window the Argon pool reports.
  const govOf = (p) => (p.windows || []).find((w) => (w.name || "") === "5 hours") || (p.windows || [])[0];

  function accountCard(r) {
    const card = el("div", "gm-card gm-account" + (picked === r.user ? " picked" : ""));
    card.tabIndex = 0;
    const go = () => {
      picked = picked === r.user ? "" : r.user;
      draw();
    };
    card.onclick = go;
    card.onkeydown = (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); go(); } };

    const pools = r.pools || [];
    const top = el("div", "gm-top");
    top.append(el("div", "gm-title", (r.user || "").split("@")[0]));
    // one badge for the account, and one below for each pool: a plan's pools
    // open on clocks of their own, so one of them being open says nothing of
    // the others
    const open = pools.filter((p) => used(govOf(p))).length;
    if (pools.length) {
      const badge = open === pools.length
        ? ["ok", t("Every pool is in use")]
        : open ? ["rest", t("{n} of {m} pools in use", { n: open, m: pools.length })] : ["rest", t("Nothing drawn on it yet")];
      top.append(el("span", "gm-badge " + badge[0], badge[1]));
    }
    card.append(top);
    card.append(el("div", "gm-sub", r.user || ""));
    if (r.err) card.append(el("div", "gm-err", r.err));
    for (const p of pools) card.append(poolBlock(p));
    card.append(watchLine(r));
    return card;
  }

  // poolBlock is one pool: its own name, the windows it has, and what the
  // watching did about it — a pool it knows no model of is drawn and marked,
  // never called.
  function poolBlock(p) {
    const box = el("div", "gm-pool");
    const top = el("div", "gm-top");
    top.append(el("div", "gm-poolname", t(p.pool || "")));
    const badge = !p.watched
      ? ["off", t("Not watched")]
      : used(govOf(p)) ? ["ok", t("Window in use")] : ["rest", t("Nothing drawn on it yet")];
    top.append(el("span", "gm-badge " + badge[0], badge[1]));
    box.append(top);
    const ws = p.windows || [];
    for (const w of ws) box.append(windowRow(w, p));
    if (!ws.length) box.append(el("div", "gm-why", t("No window of this pool was read.")));
    box.append(poolLine(p));
    if (p.err && !p.status) box.append(el("div", "gm-why", p.err));
    return box;
  }

  // windowRow is one window of a pool: what it has spent, its meter, and when
  // it starts over. A 7-day window says what it is to the watching: read
  // beside the 5-hour one, or the only window the pool has.
  function windowRow(w, p) {
    const row = el("div", "gm-win");
    const head = el("div", "gm-whead");
    head.append(el("span", "gm-wname", t(w.name || "")));
    head.append(el("span", "gm-wuse", quotaText(w)));
    row.append(head);

    const share = w.unlimited ? 0 : Math.max(0, Math.min(100, w.used || 0));
    const track = el("div", "gm-track");
    const fill = el("i");
    fill.style.width = share + "%";
    if (!used(w)) fill.classList.add("ok");
    track.append(fill);
    row.append(track);

    const line = el("div", "gm-reset");
    if (!used(w)) line.append(el("span", "", t("Nothing has been drawn on this window yet.")));
    else if (w.resetsAt) line.append(el("span", "", resetText(new Date(w.resetsAt), new Date(w.resetsAt).toLocaleString(intlLang()))));
    else line.append(el("span", "", t("Reset time not known")));
    row.append(line);

    if ((w.name || "") === "7 days") {
      const five = (p.windows || []).some((x) => (x.name || "") === "5 hours");
      row.append(el("div", "gm-why", five
        ? t("A pool's 5-hour window is what opens it; this one is read beside it, and resets with it.")
        : t("This is the pool's only window: it is what opens the pool.")));
    }
    return row;
  }

  // poolLine is what the watching last did about one pool, and the model of
  // that pool it called.
  function poolLine(p) {
    const line = el("div", "gm-reset");
    const when = [];
    if (p.model) when.push(t("Model {model}", { model: p.model }));
    if (p.lastCallAt) when.push(t("Opened {when}", { when: new Date(p.lastCallAt).toLocaleString(intlLang()) }));
    else when.push(t("Not opened yet"));
    line.append(el("span", "", when.join(" · ")));
    if (p.attempts) line.append(el("span", "gm-left", t("Tried {n} times", { n: p.attempts })));
    if (p.status) {
      const said = p.err ? p.status + " · " + p.err : String(p.status);
      line.append(el("span", "gm-left", t("First call: {said}", { said })));
    }
    return line;
  }

  // watchLine is when the account was last looked at, and when it is again:
  // one look reads every pool of it.
  function watchLine(r) {
    const line = el("div", "gm-reset");
    const when = [];
    if (r.lastCheck) when.push(t("Last look {when}", { when: new Date(r.lastCheck).toLocaleString(intlLang()) }));
    if (r.nextAt) when.push(t("Next look {when}", { when: new Date(r.nextAt).toLocaleString(intlLang()) }));
    line.append(el("span", "", when.join(" · ") || t("Not looked at yet")));
    return line;
  }

  // records is the view under the cards: the Usage page's own Requests
  // tab, so the totals, the trend, the heatmap and the list are what that
  // page draws, narrowed to the account picked and the window below.
  function records() {
    return callList();
  }

  // callList is that view's head: whose calls they are, which window they
  // are counted in, and the way back to every account. The window is the
  // one the watching follows (5 hours) or the one it only draws (7 days).
  function callList() {
    const box = el("div", "gm-records");
    const head = el("div", "row-head");
    head.append(el("span", "label", picked ? picked.split("@")[0] : t("All Antigravity accounts")));
    head.append(el("span", "grow"));
    const seg = el("div", "segs");
    for (const [id, name] of [["5h", "5 hours"], ["7d", "7 days"]]) {
      const b = el("button", "opt" + (win === id ? " on" : ""), t(name));
      b.type = "button";
      b.onclick = () => { win = id; draw(); };
      seg.append(b);
    }
    head.append(seg);
    if (picked) {
      const clear = el("button", "text", t("Show all accounts"));
      clear.type = "button";
      clear.onclick = () => { picked = ""; draw(); };
      head.append(clear);
    }
    box.append(head);
    box.append(usageFrame());
    return box;
  }

  // usageFrame is the Usage page's Requests tab as an iframe: the same code
  // draws it, so the figures are what that page shows, narrowed to this
  // account and this window by the address (usage/page.js).
  function usageFrame() {
    const wrap = el("div", "gm-frame");
    const params = { view: "usage", tab: "requests", embed: "1", period: periodOf(win) };
    if (picked) params.account = picked;
    const f = el("iframe", "gm-iframe");
    f.src = "/?" + new URLSearchParams(params).toString();
    f.loading = "lazy";
    f.title = t("Requests");
    f.scrolling = "no";
    // the frame's height is the page's own, which the first load may not
    // know yet: ask again once the fonts and the ledger have landed
    const fit = () => {
      try {
        const doc = f.contentDocument || f.contentWindow?.document;
        if (!doc) return;
        const h = Math.max(doc.body?.scrollHeight || 0, doc.documentElement?.scrollHeight || 0);
        if (h > 100) f.style.height = (h + 30) + "px";
      } catch {}
    };
    f.onload = () => {
      fit();
      setTimeout(fit, 800);
      setTimeout(fit, 2500);
    };
    wrap.append(f);
    return wrap;
  }

  ctx.slots.add("page", {
    key: "gemini",
    title: "Gemini",
    html: "",
    mount(el0) {
      main = el0;
      el0.classList.add("gm-view");
    },
    open() {
      read(true);
      clearInterval(timer);
      timer = setInterval(() => { if (shown() && !document.hidden) read(true); }, 30_000);
    },
    leave() { clearInterval(timer); },
    relang() { draw(); },
  });
});
