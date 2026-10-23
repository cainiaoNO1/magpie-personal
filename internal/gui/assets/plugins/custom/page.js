// Custom: WorkBuddy's free models on a schedule. What each probe
// answered and when, the accounts it asks, and the mode it runs in — a
// window used up is asked again once its reset has come, and an account
// that answers is asked every interval set here.
//
// A page the Custom plugin adds to the window (kernel.js), its words in
// i18n.js beside this file. Classic script, loaded before app.js: what
// runs as it loads touches neither the page nor app.js. mount, at
// magpie.start, is handed the page; open reads it, and the page keeps
// itself while it is shown.
magpie.plugin("custom", (ctx) => {
  let state = null; // /api/workbuddy/custom
  let probes = []; // /api/workbuddy/probes, newest first
  let picked = ""; // the account the cards were narrowed to
  let calls = null; // /api/workbuddy/requests of the account picked
  let kind = ""; // which calls the list keeps: "" both, "probe", "use"
  let failed = "";
  let loading = null;
  let timer = 0;

  let main = null; // the page's own element, handed to mount
  const root = () => main;
  const shown = () => root() && !root().hidden;

  // fmtEvery says an interval the way the reader set it: minutes, or
  // hours past the first.
  const fmtEvery = (sec) => {
    if (!sec) return t("magpie's own ({n} min)", { n: Math.round((state?.default || 1800) / 60) });
    if (sec % 3600 === 0) return t("{n} h", { n: sec / 3600 });
    return t("{n} min", { n: Math.round(sec / 60) });
  };

  // key is an account and a model, as the API keeps an interval.
  const key = (user, model) => user + "\u0000" + model;

  async function read(force) {
    if (loading && !force) return loading;
    loading = Promise.all([api("workbuddy/custom"), api("workbuddy/probes?limit=200"), readCalls()])
      .then(([c, p]) => { state = c; probes = Array.isArray(p) ? p : []; failed = ""; })
      .catch((e) => { failed = e.message; })
      .finally(() => { loading = null; draw(); });
    return loading;
  }

  // readCalls reads the account picked's own calls: its window, and
  // every request inside it. No account picked reads none.
  function readCalls() {
    if (!picked) { calls = null; return Promise.resolve(null); }
    return api("workbuddy/requests?user=" + encodeURIComponent(picked) + (kind ? "&kind=" + kind : ""))
      .then((c) => { calls = c; })
      .catch((e) => { failed = e.message; });
  }

  // save writes the mode back, and draws what came of it.
  async function save(patch) {
    const s = state || {};
    const body = {
      on: patch.on ?? s.on,
      models: patch.models ?? s.models,
      every: patch.every ?? s.every,
      per: patch.per ?? s.per ?? {},
    };
    try {
      state = await api("workbuddy/custom", body);
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
    h.append(el("span", "label", t("Custom")));
    h.append(el("span", "note", t("WorkBuddy's free models, asked on a schedule")));
    h.append(el("span", "grow"));
    const again = el("button", "text", t("Refresh"));
    again.type = "button";
    again.onclick = () => { again.classList.add("busy"); read(true).finally(() => again.classList.remove("busy")); };
    h.append(again);
    return h;
  }

  // modeCard is the setting: on, the interval an account that answers is
  // asked at, and the models watched.
  function modeCard() {
    const card = el("div", "cu-card");
    const top = el("div", "cu-top");
    const title = el("div", "cu-title", t("Scheduled requests"));
    const sw = el("button", "cu-switch" + (state.on ? " on" : ""), state.on ? t("On") : t("Off"));
    sw.type = "button";
    sw.setAttribute("aria-pressed", String(!!state.on));
    sw.onclick = () => save({ on: !state.on });
    top.append(title, sw);
    card.append(top);
    card.append(el("div", "cu-sub", t("A window used up is asked again once its reset has come, and counted back in only on a 200. An account that answers is asked every interval below.")));

    // the interval every account without its own is asked at
    const every = el("div", "cu-row");
    every.append(el("span", "cu-k", t("Every account that answers")));
    const seg = el("div", "segs");
    for (const [sec, name] of [[0, t("magpie's own")], [300, "5 min"], [600, "10 min"], [1800, "30 min"], [3600, "1 h"]]) {
      const b = el("button", "opt" + ((state.every || 0) === sec ? " on" : ""), name);
      b.type = "button";
      b.onclick = () => save({ every: sec });
      seg.append(b);
    }
    every.append(seg);
    card.append(every);

    // which models are watched on every account
    const models = el("div", "cu-row");
    models.append(el("span", "cu-k", t("Models watched")));
    const box = el("div", "cu-models");
    for (const m of state.served || []) {
      const on = (state.models || []).includes(m);
      const b = el("button", "cu-chip" + (on ? " on" : ""), m);
      b.type = "button";
      b.onclick = () => {
        const now = new Set(state.models || []);
        on ? now.delete(m) : now.add(m);
        save({ models: [...now] });
      };
      box.append(b);
    }
    models.append(box);
    card.append(models);

    if (failed) card.append(el("div", "cu-err", failed));
    return card;
  }

  // accounts draws one card an account, its models inside: the cards
  // narrow the records below to that account.
  function accounts() {
    const rows = state.rows || [];
    const wrap = el("div", "cu-accounts");
    const users = [];
    for (const r of rows) if (!users.includes(r.user)) users.push(r.user);
    for (const user of users) {
      const list = rows.filter((r) => r.user === user);
      const card = el("div", "cu-card cu-account" + (picked === user ? " picked" : ""));
      card.tabIndex = 0;
      const go = () => {
        picked = picked === user ? "" : user;
        kind = "";
        calls = null;
        draw();
        if (picked) readCalls().then(draw);
      };
      card.onclick = go;
      card.onkeydown = (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); go(); } };

      const top = el("div", "cu-top");
      top.append(el("div", "cu-title", user.split("@")[0]));
      const ready = list.some((r) => r.healthy);
      top.append(el("span", "cu-badge " + (ready ? "ok" : "rest"), ready ? t("Ready") : t("Used up")));
      card.append(top);

      for (const r of list) card.append(modelRow(r));
      wrap.append(card);
    }
    return wrap;
  }

  // modelRow is one model of an account: its state, what the window has
  // spent, when it starts over, and its own interval.
  function modelRow(r) {
    const row = el("div", "cu-model");
    const head = el("div", "cu-mhead");
    head.append(el("span", "cu-mname", r.model));
    const own = (state.per || {})[key(r.user, r.model)];
    const every = el("button", "text cu-every", own ? fmtEvery(own) : fmtEvery(state.every));
    every.type = "button";
    every.title = t("Ask this account and model this often while it answers");
    every.onclick = (ev) => {
      ev.stopPropagation();
      const pick = prompt(t("Minutes between requests, 0 for the setting's own"), String(Math.round((own || state.every || state.default || 1800) / 60)));
      if (pick == null) return;
      const mins = Math.max(0, Math.round(Number(pick) || 0));
      const per = { ...(state.per || {}) };
      if (mins > 0) per[key(r.user, r.model)] = mins * 60;
      else delete per[key(r.user, r.model)];
      save({ per });
    };
    head.append(every);
    row.append(head);

    const share = r.limit > 0 ? Math.min(100, (r.spent / r.limit) * 100) : 0;
    const labels = el("div", "cu-labels");
    labels.append(el("span", "", t("This window")));
    labels.append(el("span", "cu-n", r.spent ? fmtN(r.spent) + " / " + fmtN(r.limit) : t("no calls yet")));
    row.append(labels);
    const track = el("div", "cu-track");
    const fill = el("i");
    fill.style.width = share + "%";
    if (r.healthy) fill.classList.add("ok");
    track.append(fill);
    row.append(track);

    if (r.firstCallAt) {
      const fLine = el("div", "cu-reset cu-firstcall");
      const fAt = new Date(r.firstCallAt);
      fLine.append(el("span", "", t("First called {when}", { when: resetClock(fAt) })));
      row.append(fLine);
    }

    if (r.until) {
      const at = new Date(r.until);
      const line = el("div", "cu-reset");
      line.append(el("span", "", at > Date.now() && !r.healthy ? resetText(at) : t("Window open")));
      if (at > Date.now() && !r.healthy) line.append(el("span", "cu-left", untilText(at)));
      row.append(line);
    }
    return row;
  }

  // records is the view under the cards: always show the usage requests view
  // (when an account is picked, filtered to that account; when none, showing all)
  function records() {
    return callList();
  }

  // probeList is every probe kept, newest first.
  function probeList() {
    const box = el("div", "cu-records");
    const head = el("div", "row-head");
    head.append(el("span", "label", t("Scheduled requests")));
    const mine = probes;
    head.append(el("span", "note", t("{n} requests", { n: mine.length })));
    box.append(head);

    if (!mine.length) {
      box.append(el("p", "usage-note", t("No requests yet. The first one goes when an account's window starts over, or at its interval.")));
      return box;
    }
    const list = el("div", "list cu-list");
    for (const p of mine.slice(0, 200)) {
      const row = el("div", "row");
      const who = el("div", "who");
      const name = el("div", "name");
      name.append(el("span", "", String(p.user || "").split("@")[0]));
      name.append(el("span", "cu-mname", p.model || ""));
      const sub = el("div", "sub");
      const when = new Date(p.at);
      sub.append(el("span", "", when.toLocaleString(intlLang())));
      if (p.frozen) sub.append(el("span", "cu-tag", t("after a window ran out")));
      if (p.ms) sub.append(el("span", "", t("{n} ms", { n: p.ms })));
      who.append(name, sub);
      row.append(who);

      const said = el("div", "cu-said");
      if (p.status === 200) said.append(el("span", "cu-badge ok", "200"));
      else if (p.status) said.append(el("span", "cu-badge rest", String(p.status)));
      else said.append(el("span", "cu-badge off", t("no answer")));
      if (p.why) {
        const why = el("div", "cu-why", String(p.why).slice(0, 240));
        why.title = p.why;
        said.append(why);
      }
      row.append(said);
      list.append(row);
    }
    box.append(list);
    return box;
  }

  // callList is the account picked's calls in its window: when, what it
  // said, and what it cost. The picks above keep the watching's own
  // probes apart from the account's use.
  function callList() {
    const box = el("div", "cu-records");
    if (picked) {
      const head = el("div", "row-head");
      head.append(el("span", "label", picked.split("@")[0]));
      if (calls) {
        head.append(el("span", "note", t("{from} to now", { from: new Date(calls.from).toLocaleString(intlLang()) })));
      }
      head.append(el("span", "grow"));
      for (const [k, name] of [["", t("All requests")], ["use", t("Use")], ["probe", t("Scheduled")]]) {
        const b = el("button", "opt" + (kind === k ? " on" : ""), name);
        b.type = "button";
        if (calls) {
          const n = k === "probe" ? calls.probe : k === "use" ? calls.use : calls.probe + calls.use;
          b.append(el("span", "cu-n", String(n)));
        }
        b.onclick = (ev) => { ev.stopPropagation(); kind = k; readCalls().then(draw); };
        head.append(b);
      }
      const clear = el("button", "text", t("Show every account"));
      clear.type = "button";
      clear.onclick = (ev) => { ev.stopPropagation(); picked = ""; calls = null; kind = ""; draw(); };
      head.append(clear);
      box.append(head);
    }

    if (picked && !calls) {
      box.append(el("p", "usage-note", t("Reading…")));
      return box;
    }
    // the Usage page's own Requests tab, embedded: the same code draws it,
    // so the figures, the trend, the heatmap and the list are what that
    // page shows, narrowed to this account and its window (or all accounts when none picked)
    box.append(usageFrame(calls));
    return box;
  }

  // usageFrame is the Usage page's Requests tab as an iframe: the account
  // picked and the window its card names, read from the address
  // (usage/page.js).
  function usageFrame(c) {
    const wrap = el("div", "cu-frame");
    const params = { view: "usage", tab: "requests", embed: "1" };
    if (picked) params.account = picked;
    const q = new URLSearchParams(params);
    if (c && c.period) q.set("period", c.period);
    const f = el("iframe", "cu-iframe");
    f.src = "/?" + q.toString();
    f.loading = "lazy";
    f.title = t("Requests");
    f.scrolling = "no";
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
    key: "custom",
    title: "Custom",
    html: "",
    mount(el0) {
      main = el0;
      el0.classList.add("cu-view");
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
