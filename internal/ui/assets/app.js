/* SableCourt Command Palette (Cmd+K / Ctrl+K) & Micro-interactions */
(function () {
  const COMMANDS = [
    { label: "Cockpit Dashboard", hint: "/", url: "/" },
    { label: "Multi-Agent Conclaves", hint: "/conclaves", url: "/conclaves" },
    { label: "Cron Schedules", hint: "/schedules", url: "/schedules" },
    { label: "Court Memory (FTS5)", hint: "/memory", url: "/memory" },
    { label: "Audit Tribunal & SHA-256 Chain", hint: "/audit", url: "/audit" },
    { label: "Workload & Conclave Studio", hint: "/studio", url: "/studio" },
    { label: "LLM Providers & Models", hint: "/providers", url: "/providers" },
    { label: "A2A Agent Card (JSON)", hint: "/.well-known/agent-card.json", url: "/.well-known/agent-card.json" },
    { label: "Prometheus Metrics", hint: "/metrics", url: "/metrics" },
    { label: "Control Plane Health", hint: "/healthz", url: "/healthz" },
    { label: "Export Audit Log (JSONL)", hint: "/api/v1/audit/export", url: "/api/v1/audit/export" }
  ];

  function ensureModal() {
    let el = document.getElementById("sc-cmdk");
    if (el) return el;
    el = document.createElement("div");
    el.id = "sc-cmdk";
    el.className = "sc-cmdk-backdrop";
    el.innerHTML =
      '<div class="sc-cmdk-dialog" role="dialog" aria-label="Command palette">' +
      '  <input id="sc-cmdk-input" class="sc-cmdk-input" type="text" placeholder="Jump to view, tribunal, memory, or endpoint..." autocomplete="off">' +
      '  <div id="sc-cmdk-list" class="sc-cmdk-list"></div>' +
      '  <div class="sc-cmdk-foot"><span>↑↓ navigate · Enter open · Esc close</span><span>SableCourt · ShadeWright Labs</span></div>' +
      '</div>';
    document.body.appendChild(el);
    el.addEventListener("click", function (ev) {
      if (ev.target === el) close();
    });
    return el;
  }

  let selected = 0;
  let filtered = COMMANDS.slice();

  function renderList() {
    const list = document.getElementById("sc-cmdk-list");
    if (!list) return;
    list.innerHTML = "";
    filtered.forEach(function (cmd, idx) {
      const a = document.createElement("a");
      a.className = "sc-cmdk-item" + (idx === selected ? " selected" : "");
      a.href = cmd.url;
      a.innerHTML = "<span>" + cmd.label + "</span><small>" + cmd.hint + "</small>";
      a.addEventListener("mouseenter", function () {
        selected = idx;
        renderList();
      });
      list.appendChild(a);
    });
  }

  function open() {
    const el = ensureModal();
    el.classList.add("open");
    const inp = document.getElementById("sc-cmdk-input");
    inp.value = "";
    filtered = COMMANDS.slice();
    selected = 0;
    renderList();
    setTimeout(function () { inp.focus(); }, 10);
  }

  function close() {
    const el = document.getElementById("sc-cmdk");
    if (el) el.classList.remove("open");
  }

  document.addEventListener("keydown", function (ev) {
    if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === "k") {
      ev.preventDefault();
      const el = document.getElementById("sc-cmdk");
      if (el && el.classList.contains("open")) close();
      else open();
      return;
    }
    const el = document.getElementById("sc-cmdk");
    if (!el || !el.classList.contains("open")) return;
    if (ev.key === "Escape") {
      ev.preventDefault();
      close();
    } else if (ev.key === "ArrowDown") {
      ev.preventDefault();
      selected = (selected + 1) % Math.max(1, filtered.length);
      renderList();
    } else if (ev.key === "ArrowUp") {
      ev.preventDefault();
      selected = (selected - 1 + Math.max(1, filtered.length)) % Math.max(1, filtered.length);
      renderList();
    } else if (ev.key === "Enter" && filtered[selected]) {
      ev.preventDefault();
      window.location.href = filtered[selected].url;
    }
  });

  document.addEventListener("input", function (ev) {
    if (ev.target && ev.target.id === "sc-cmdk-input") {
      const q = ev.target.value.trim().toLowerCase();
      filtered = COMMANDS.filter(function (c) {
        return !q || c.label.toLowerCase().includes(q) || c.hint.toLowerCase().includes(q);
      });
      selected = 0;
      renderList();
    }
  });

  document.addEventListener("click", function (ev) {
    const btn = ev.target.closest && ev.target.closest("[data-open-cmdk]");
    if (btn) {
      ev.preventDefault();
      open();
    }
  });
})();
