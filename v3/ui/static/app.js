(function () {
  "use strict";

  const state = {
    authenticated: false,
    authRequired: false,
    subject: "local",
    csrfToken: "",
    activeView: "agents",
    selectedAgent: "main",
    stopTargetAgent: "",
    agents: [],
    sessions: [],
    pages: [],
    secrets: [],
    permissions: [],
    requests: [],
    batchesPending: [],
    batchesAll: [],
    batchDetails: {},
    auditEntries: [],
    auditVerify: { ok: true, entries: 0 },
    systemInfo: { snapshot: "none (no snapshot available — not undoable)" },
    ws: null,
    wsAgent: "",
    transcriptEvents: [],
    pollTimer: null
  };

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  function showToast(msg, isError) {
    const t = document.getElementById("global-toast");
    if (!t) return;
    t.textContent = msg;
    t.classList.remove("hidden");
    t.style.borderColor = isError ? "var(--danger)" : "var(--accent)";
    clearTimeout(t._timer);
    t._timer = setTimeout(function () {
      t.classList.add("hidden");
    }, 3500);
  }

  async function apiFetch(path, options) {
    const opts = Object.assign({ method: "GET", credentials: "same-origin" }, options || {});
    opts.headers = Object.assign({}, opts.headers || {});
    if (opts.body && typeof opts.body === "object" && !(opts.body instanceof FormData)) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(opts.body);
    }
    if (opts.method !== "GET" && opts.method !== "HEAD" && state.csrfToken) {
      opts.headers["X-Silo-CSRF"] = state.csrfToken;
    }
    const resp = await fetch(path, opts);
    let data = null;
    const ct = resp.headers.get("Content-Type") || "";
    if (ct.includes("application/json")) {
      try {
        data = await resp.json();
      } catch (_) {
        data = null;
      }
    }
    if (resp.status === 401 && path !== "/api/v1/auth/login" && path !== "/api/v1/auth/session") {
      state.authenticated = false;
      showLoginModal(true);
      throw new Error((data && data.error) || "Authentication required");
    }
    if (!resp.ok) {
      throw new Error((data && data.error) || ("HTTP " + resp.status));
    }
    return data;
  }

  function showLoginModal(show) {
    const modal = document.getElementById("login-overlay");
    if (!modal) return;
    if (show) {
      modal.classList.remove("hidden");
      const inp = document.getElementById("login-token-input");
      if (inp) inp.focus();
    } else {
      modal.classList.add("hidden");
    }
  }

  async function bootstrapAuth() {
    try {
      const sess = await apiFetch("/api/v1/auth/session");
      state.authenticated = Boolean(sess && sess.authenticated);
      state.authRequired = Boolean(sess && sess.auth_required);
      state.subject = (sess && sess.subject) || "local";
      state.csrfToken = (sess && sess.csrf_token) || "";
      document.getElementById("auth-subject-pill").textContent = "auth: " + state.subject;
      const logoutBtn = document.getElementById("logout-btn");
      if (state.authRequired && state.authenticated) {
        logoutBtn.classList.remove("hidden");
      } else {
        logoutBtn.classList.add("hidden");
      }
      if (!state.authenticated) {
        showLoginModal(true);
        return false;
      }
      showLoginModal(false);
      return true;
    } catch (_) {
      showLoginModal(true);
      return false;
    }
  }

  async function refreshAll() {
    if (!state.authenticated) return;
    const showInactivePages = document.getElementById("pages-show-inactive")?.checked;
    try {
      const [agents, sessions, pages, secrets, perms, reqs, batchesAll, auditEntries, auditVerify, sysInfo] = await Promise.all([
        apiFetch("/api/v1/agents").catch(function () { return []; }),
        apiFetch("/api/v1/sessions").catch(function () { return []; }),
        apiFetch("/api/v1/pages" + (showInactivePages ? "?all=true" : "")).catch(function () { return []; }),
        apiFetch("/api/v1/secrets").catch(function () { return []; }),
        apiFetch("/api/v1/permissions").catch(function () { return []; }),
        apiFetch("/api/v1/requests").catch(function () { return []; }),
        apiFetch("/api/v1/approvals?all=true").catch(function () { return []; }),
        apiFetch("/api/v1/audit?limit=250").catch(function () { return []; }),
        apiFetch("/api/v1/audit/verify").catch(function () { return { ok: true, entries: 0 }; }),
        apiFetch("/api/v1/system").catch(function () { return null; })
      ]);

      state.agents = Array.isArray(agents) ? agents : [];
      state.sessions = Array.isArray(sessions) ? sessions : [];
      state.pages = Array.isArray(pages) ? pages : [];
      state.secrets = Array.isArray(secrets) ? secrets : [];
      state.permissions = Array.isArray(perms) ? perms : [];
      state.requests = Array.isArray(reqs) ? reqs : [];
      state.batchesAll = Array.isArray(batchesAll) ? batchesAll : [];
      state.batchesPending = state.batchesAll.filter(function (b) { return b.status === "pending"; });
      state.auditEntries = Array.isArray(auditEntries) ? auditEntries : [];
      state.auditVerify = auditVerify || { ok: true, entries: 0 };
      if (sysInfo) state.systemInfo = sysInfo;

      // Fetch full presentation details for every batch so pending & decided batches render complete facts/diffs/steps.
      await Promise.all(state.batchesAll.map(async function (b) {
        try {
          const d = await apiFetch("/api/v1/approvals/" + encodeURIComponent(b.batch_id));
          if (d) state.batchDetails[b.batch_id] = d;
        } catch (_) {}
      }));

      updateHeaderBadges();
      renderAgentsTree();
      renderPagesView();
      renderSessionView();
      renderApprovalsView();
      renderAuditView();
    } catch (err) {
      console.error("refreshAll:", err);
    }
  }

  function updateHeaderBadges() {
    document.getElementById("badge-agents").textContent = String(state.agents.length);
    const activePages = state.pages.filter(function (p) { return p.active; });
    document.getElementById("badge-pages").textContent = String(activePages.length);
    document.getElementById("badge-session-agent").textContent = state.selectedAgent || "main";

    const pendSecrets = state.secrets.filter(function (s) { return s.state === "pending"; }).length;
    const pendPerms = state.permissions.filter(function (p) { return p.state === "pending"; }).length;
    const pendReqs = state.requests.filter(function (r) { return r.state === "pending"; }).length;
    const totalPending = state.batchesPending.length + pendSecrets + pendPerms + pendReqs;

    const appBadge = document.getElementById("badge-approvals");
    appBadge.textContent = String(totalPending);
    appBadge.className = "badge " + (totalPending > 0 ? "badge-danger" : "badge-neutral");

    const auditBadge = document.getElementById("badge-audit");
    if (state.auditVerify && state.auditVerify.ok === false) {
      auditBadge.textContent = "FAIL";
      auditBadge.className = "badge badge-danger";
    } else {
      auditBadge.textContent = "OK (" + ((state.auditVerify && state.auditVerify.entries) || state.auditEntries.length) + ")";
      auditBadge.className = "badge badge-ok";
    }

    const snapPill = document.getElementById("system-snapshot-pill");
    if (state.systemInfo && state.systemInfo.snapshot_available) {
      snapPill.textContent = state.systemInfo.snapshot || "Snapshot: available";
      snapPill.className = "status-pill badge-ok";
    } else {
      snapPill.textContent = "No snapshot available — not undoable";
      snapPill.className = "status-pill badge-warning";
    }

    // Update parent dropdown in Register Agent form
    const parentSelect = document.getElementById("reg-agent-parent");
    if (parentSelect) {
      const currentVal = parentSelect.value;
      parentSelect.innerHTML = "";
      const noneOpt = el("option", "", "(none — top-level)");
      noneOpt.value = "";
      parentSelect.appendChild(noneOpt);
      state.agents.forEach(function (a) {
        if (a.state !== "stopped") {
          const opt = el("option", "", a.id);
          opt.value = a.id;
          parentSelect.appendChild(opt);
        }
      });
      parentSelect.value = currentVal || "main";
    }
  }

  function combinedAgentStatus(agent) {
    if (agent.state !== "connected") {
      return agent.state;
    }
    const sess = state.sessions.find(function (s) { return s.agent === agent.id; });
    if (!sess) return "connected · idle";
    if (sess.state === "busy") return "connected · running";
    if (sess.state === "ready") return "connected · idle";
    return "connected · " + sess.state;
  }

  function statusBadgeClass(agent) {
    if (agent.state === "connected") {
      const sess = state.sessions.find(function (s) { return s.agent === agent.id; });
      return (sess && sess.state === "busy") ? "badge-info" : "badge-ok";
    }
    if (agent.state === "registered") return "badge-info";
    if (agent.state === "pending") return "badge-warning";
    return "badge-neutral";
  }

  function buildPageChip(pg) {
    if (pg.url) {
      const a = el("a", "page-chip");
      a.href = pg.url;
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      a.appendChild(el("span", "bold", pg.title || pg.slug));
      a.appendChild(el("span", "mono", "(" + pg.url + " ↗)"));
      return a;
    }
    const wrap = el("span", "page-chip-group");
    const a = el("a", "page-chip");
    a.href = "/p/" + encodeURIComponent(pg.slug) + "/";
    a.target = "_blank";
    a.rel = "noopener noreferrer";
    a.appendChild(el("span", "bold", pg.title || pg.slug));
    a.appendChild(el("span", "mono", "/p/" + pg.slug + "/ (:" + pg.port + ")"));
    wrap.appendChild(a);
    if (pg.expose_port) {
      const exp = el("a", "page-chip");
      exp.href = "http://127.0.0.1:" + pg.expose_port + "/";
      exp.target = "_blank";
      exp.rel = "noopener noreferrer";
      exp.textContent = "exposed :" + pg.expose_port;
      wrap.appendChild(exp);
    }
    return wrap;
  }

  // ---- 1. AGENTS & SESSIONS TREE VIEW ----
  function renderAgentsTree() {
    const container = document.getElementById("agents-tree-container");
    if (!container) return;
    container.innerHTML = "";

    if (state.agents.length === 0) {
      container.appendChild(el("div", "card muted-text", "No agents registered yet."));
      return;
    }

    const byID = {};
    state.agents.forEach(function (a) { byID[a.id] = a; });

    const childrenMap = {};
    const roots = [];
    state.agents.forEach(function (a) {
      const p = a.parent || "";
      if (!p || !byID[p]) {
        roots.push(a);
      } else {
        if (!childrenMap[p]) childrenMap[p] = [];
        childrenMap[p].push(a);
      }
    });

    roots.sort(function (a, b) {
      if (a.root !== b.root) return a.root ? -1 : 1;
      return a.id.localeCompare(b.id);
    });

    function renderNode(agent, depth) {
      const card = el("div", "agent-node" + (agent.root ? " agent-root" : "") + (agent.state === "stopped" ? " agent-stopped" : ""));
      card.style.marginLeft = (depth * 28) + "px";
      card.setAttribute("data-agent-id", agent.id);

      const top = el("div", "agent-node-top");
      const titleGroup = el("div", "agent-node-title");
      if (depth > 0) {
        titleGroup.appendChild(el("span", "mono muted-text", "↳"));
      }
      titleGroup.appendChild(el("span", "agent-id-name", agent.id));

      if (agent.root) {
        titleGroup.appendChild(el("span", "badge badge-warning", "root bootstrapper"));
      } else if (agent.standing) {
        titleGroup.appendChild(el("span", "badge badge-info", "standing"));
      }

      titleGroup.appendChild(el("span", "badge " + statusBadgeClass(agent), combinedAgentStatus(agent)));

      const locKind = (agent.location && agent.location.kind) || "other";
      const locDetail = (agent.location && agent.location.detail) ? (" (" + agent.location.detail + ")") : "";
      titleGroup.appendChild(el("span", "badge badge-neutral", "location: " + locKind + locDetail));

      if (agent.runtime) {
        titleGroup.appendChild(el("span", "badge badge-neutral", "cli: " + agent.runtime));
      }
      if (agent.user) {
        titleGroup.appendChild(el("span", "badge badge-neutral", "user: " + agent.user));
      }

      // Pending counts badge for this agent
      const agBatches = state.batchesPending.filter(function (b) { return b.submitter === agent.id; }).length;
      const agSecrets = state.secrets.filter(function (s) { return s.agent === agent.id && s.state === "pending"; }).length;
      const agPerms = state.permissions.filter(function (p) { return p.agent === agent.id && p.state === "pending"; }).length;
      const agReqs = state.requests.filter(function (r) { return r.agent === agent.id && r.state === "pending"; }).length;
      const agPendingTotal = agBatches + agSecrets + agPerms + agReqs;

      if (agPendingTotal > 0) {
        const pendBtn = el("button", "badge badge-danger", agPendingTotal + " pending approval/secret/request");
        pendBtn.style.cursor = "pointer";
        pendBtn.addEventListener("click", function () {
          switchView("approvals");
        });
        titleGroup.appendChild(pendBtn);
      }

      top.appendChild(titleGroup);

      // Right-hand controls on agent node
      const actions = el("div", "header-actions");

      // Permission policy selector
      const permSelect = el("select", "input-field mono");
      permSelect.style.width = "auto";
      permSelect.style.padding = "3px 8px";
      permSelect.title = "Agent permission policy";
      ["ask", "auto", "deny-shell"].forEach(function (mode) {
        const o = el("option", "", "perm: " + mode);
        o.value = mode;
        if (agent.permissions === mode) o.selected = true;
        permSelect.appendChild(o);
      });
      permSelect.addEventListener("change", async function () {
        try {
          await apiFetch("/api/v1/agents/" + encodeURIComponent(agent.id) + "/permissions", {
            method: "POST",
            body: { permissions: permSelect.value }
          });
          showToast("Updated " + agent.id + " permissions to " + permSelect.value);
          await refreshAll();
        } catch (err) {
          showToast(err.message, true);
          await refreshAll();
        }
      });
      actions.appendChild(permSelect);

      if (agent.state === "pending") {
        const approveBtn = el("button", "btn btn-sm btn-primary", "Approve Child");
        approveBtn.addEventListener("click", async function () {
          try {
            await apiFetch("/api/v1/agents/" + encodeURIComponent(agent.id) + "/approve", {
              method: "POST",
              body: { approved: true }
            });
            showToast("Approved agent " + agent.id);
            await refreshAll();
          } catch (err) {
            showToast(err.message, true);
          }
        });
        actions.appendChild(approveBtn);
      }

      const chatBtn = el("button", "btn btn-sm btn-secondary", "Session / Chat");
      chatBtn.addEventListener("click", function () {
        openAgentSession(agent.id);
      });
      actions.appendChild(chatBtn);

      if (agent.state !== "stopped") {
        const stopBtn = el("button", "btn btn-sm btn-danger", "Stop");
        stopBtn.addEventListener("click", function () {
          openStopModal(agent);
        });
        actions.appendChild(stopBtn);
      }

      top.appendChild(actions);
      card.appendChild(top);

      // Metadata line
      const meta = el("div", "agent-meta-row mono");
      if (agent.parent) meta.appendChild(el("span", "", "parent: " + agent.parent));
      if (agent.limits) {
        const limParts = [];
        if (agent.limits.memory_max) limParts.push("mem=" + agent.limits.memory_max);
        if (agent.limits.cpu_quota) limParts.push("cpu=" + agent.limits.cpu_quota);
        if (agent.limits.tasks_max) limParts.push("tasks=" + agent.limits.tasks_max);
        if (limParts.length > 0) meta.appendChild(el("span", "", "limits: " + limParts.join(" ")));
      }
      if (agent.may_register) meta.appendChild(el("span", "", "may_register=true"));
      if (agent.brief) meta.appendChild(el("span", "", "brief: \"" + agent.brief + "\""));
      if (agent.report) {
        meta.appendChild(el("span", "bold", "report (" + agent.report.status + "): " + agent.report.summary));
      }
      if (agent.tombstone) {
        meta.appendChild(el("span", "", "stopped: " + agent.tombstone.cause));
      }
      card.appendChild(meta);

      // Agent's registered page links
      const agentPages = state.pages.filter(function (p) { return p.owner === agent.id && p.active; });
      if (agentPages.length > 0) {
        const pagesRow = el("div", "agent-pages-row");
        pagesRow.appendChild(el("span", "muted-text small bold", "Pages:"));
        agentPages.forEach(function (pg) {
          pagesRow.appendChild(buildPageChip(pg));
        });
        card.appendChild(pagesRow);
      }

      container.appendChild(card);

      const kids = childrenMap[agent.id] || [];
      kids.sort(function (a, b) { return a.id.localeCompare(b.id); });
      kids.forEach(function (ch) { renderNode(ch, depth + 1); });
    }

    roots.forEach(function (r) { renderNode(r, 0); });
  }

  function openStopModal(agent) {
    state.stopTargetAgent = agent.id;
    const desc = document.getElementById("stop-modal-desc");
    const kids = (agent.children && agent.children.length > 0) ? (" and its descendants (" + agent.children.join(", ") + ")") : "";
    desc.textContent = "Stop agent \"" + agent.id + "\"" + kids + "? This signals its `silo connect` wrapper, revokes its session token, deactivates its page links, and stops its registered subtree.";
    document.getElementById("stop-modal").classList.remove("hidden");
  }

  // ---- 2. PAGES VIEW ----
  function renderPagesView() {
    const internalList = document.getElementById("internal-pages-list");
    const externalList = document.getElementById("external-pages-list");
    if (!internalList || !externalList) return;
    internalList.innerHTML = "";
    externalList.innerHTML = "";

    const internalPages = state.pages.filter(function (p) { return !p.url; });
    const externalPages = state.pages.filter(function (p) { return Boolean(p.url); });

    document.getElementById("internal-pages-count").textContent = String(internalPages.length);
    document.getElementById("external-pages-count").textContent = String(externalPages.length);

    if (internalPages.length === 0) {
      internalList.appendChild(el("div", "muted-text", "No internal service page links registered."));
    } else {
      internalPages.forEach(function (pg) {
        const item = el("div", "page-item-card");
        const left = el("div", "");
        const titleRow = el("div", "agent-node-title");
        titleRow.appendChild(el("span", "bold", pg.title || pg.slug));
        const slugLink = el("a", "mono page-chip", "/p/" + pg.slug + "/");
        slugLink.href = "/p/" + encodeURIComponent(pg.slug) + "/";
        slugLink.target = "_blank";
        slugLink.rel = "noopener noreferrer";
        titleRow.appendChild(slugLink);
        titleRow.appendChild(el("span", "badge " + (pg.active ? "badge-ok" : "badge-neutral"), pg.active ? "active" : "inactive"));
        left.appendChild(titleRow);

        const meta = el("div", "page-item-meta mono");
        const host = pg.host || "127.0.0.1";
        let metaText = "owner: " + (pg.owner || "human") + " · target: " + host + ":" + pg.port;
        if (pg.expose_port) {
          metaText += " · exposed port: :" + pg.expose_port;
        }
        meta.textContent = metaText;
        left.appendChild(meta);
        item.appendChild(left);

        const right = el("div", "header-actions");
        const openBtn = el("a", "btn btn-sm btn-secondary", "Open /p/" + pg.slug + "/");
        openBtn.href = "/p/" + encodeURIComponent(pg.slug) + "/";
        openBtn.target = "_blank";
        openBtn.rel = "noopener noreferrer";
        right.appendChild(openBtn);

        if (pg.active) {
          const rmBtn = el("button", "btn btn-sm btn-danger", "Remove");
          rmBtn.addEventListener("click", async function () {
            try {
              await apiFetch("/api/v1/pages/" + encodeURIComponent(pg.slug), { method: "DELETE" });
              showToast("Removed page link /p/" + pg.slug + "/");
              await refreshAll();
            } catch (err) {
              showToast(err.message, true);
            }
          });
          right.appendChild(rmBtn);
        }
        item.appendChild(right);
        internalList.appendChild(item);
      });
    }

    if (externalPages.length === 0) {
      externalList.appendChild(el("div", "muted-text", "No external HTTP(S) page links registered."));
    } else {
      externalPages.forEach(function (pg) {
        const item = el("div", "page-item-card");
        const left = el("div", "");
        const titleRow = el("div", "agent-node-title");
        titleRow.appendChild(el("span", "bold", pg.title || pg.slug));
        titleRow.appendChild(el("span", "mono muted-text", "slug: " + pg.slug));
        titleRow.appendChild(el("span", "badge " + (pg.active ? "badge-ok" : "badge-neutral"), pg.active ? "active" : "inactive"));
        left.appendChild(titleRow);

        const urlRow = el("div", "page-item-meta mono");
        urlRow.appendChild(el("span", "", "owner: " + (pg.owner || "human") + " · destination: "));
        const extAnchor = el("a", "mono bold", pg.url);
        extAnchor.href = pg.url;
        extAnchor.setAttribute("target", "_blank");
        extAnchor.setAttribute("rel", "noopener noreferrer"); // target="_blank" rel="noopener noreferrer"
        extAnchor.style.color = "var(--accent-hover)";
        urlRow.appendChild(extAnchor);
        left.appendChild(urlRow);
        item.appendChild(left);

        const right = el("div", "header-actions");
        const openBtn = el("a", "btn btn-sm btn-secondary", "Open External ↗");
        openBtn.href = pg.url;
        openBtn.target = "_blank";
        openBtn.rel = "noopener noreferrer";
        right.appendChild(openBtn);

        if (pg.active) {
          const rmBtn = el("button", "btn btn-sm btn-danger", "Remove");
          rmBtn.addEventListener("click", async function () {
            try {
              await apiFetch("/api/v1/pages/" + encodeURIComponent(pg.slug), { method: "DELETE" });
              showToast("Removed external link " + pg.slug);
              await refreshAll();
            } catch (err) {
              showToast(err.message, true);
            }
          });
          right.appendChild(rmBtn);
        }
        item.appendChild(right);
        externalList.appendChild(item);
      });
    }
  }

  // ---- 3. SESSION VIEW (CHAT, TRANSCRIPT, INLINE PERMISSIONS & SECRETS) ----
  function openAgentSession(agentID) {
    state.selectedAgent = agentID;
    switchView("session");
    connectSessionStream(agentID);
    renderSessionView();
  }

  function connectSessionStream(agentID) {
    if (!agentID) return;
    if (state.ws && state.wsAgent === agentID && state.ws.readyState <= 1) {
      return;
    }
    if (state.ws) {
      try { state.ws.close(); } catch (_) {}
      state.ws = null;
    }
    state.wsAgent = agentID;
    state.transcriptEvents = [];
    const statusBadge = document.getElementById("ws-stream-status");
    if (statusBadge) {
      statusBadge.textContent = "WS: connecting…";
      statusBadge.className = "badge badge-info";
    }

    // Load initial transcript via HTTP first so history is visible even if agent is offline.
    apiFetch("/api/v1/sessions/" + encodeURIComponent(agentID) + "/transcript")
      .then(function (events) {
        if (state.selectedAgent !== agentID) return;
        if (Array.isArray(events)) {
          state.transcriptEvents = events.map(function (ev) { return ev.message || ev; });
          renderTranscriptDom();
        }
      })
      .catch(function () {});

    const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
    const wsURL = proto + "//" + window.location.host + "/acp/v1/sessions/" + encodeURIComponent(agentID) + "?history=false";
    try {
      const ws = new WebSocket(wsURL);
      state.ws = ws;
      ws.onopen = function () {
        if (statusBadge) {
          statusBadge.textContent = "WS: live (" + agentID + ")";
          statusBadge.className = "badge badge-ok";
        }
      };
      ws.onmessage = function (ev) {
        try {
          const msg = JSON.parse(ev.data);
          state.transcriptEvents.push(msg);
          renderTranscriptDom();
        } catch (_) {}
      };
      ws.onclose = function () {
        if (state.ws === ws && statusBadge) {
          statusBadge.textContent = "WS: disconnected";
          statusBadge.className = "badge badge-neutral";
        }
      };
    } catch (_) {}
  }

  function renderSessionView() {
    const selector = document.getElementById("session-agent-selector");
    const headerBar = document.getElementById("session-header-bar");
    const inlineSecrets = document.getElementById("session-inline-secrets");
    const inlinePerms = document.getElementById("session-inline-permissions");
    if (!selector || !headerBar || !inlineSecrets || !inlinePerms) return;

    selector.innerHTML = "";
    state.agents.forEach(function (a) {
      const btn = el("button", "agent-select-btn" + (a.id === state.selectedAgent ? " active" : ""));
      btn.appendChild(el("span", "bold", a.id));
      btn.appendChild(el("span", "badge " + statusBadgeClass(a), a.state));
      btn.addEventListener("click", function () {
        openAgentSession(a.id);
      });
      selector.appendChild(btn);
    });

    const curAgent = state.agents.find(function (a) { return a.id === state.selectedAgent; }) || state.agents[0];
    if (curAgent && state.selectedAgent !== curAgent.id) {
      state.selectedAgent = curAgent.id;
    }

    headerBar.innerHTML = "";
    if (curAgent) {
      const top = el("div", "agent-node-top");
      const left = el("div", "agent-node-title");
      left.appendChild(el("span", "agent-id-name", "Session: " + curAgent.id));
      left.appendChild(el("span", "badge " + statusBadgeClass(curAgent), combinedAgentStatus(curAgent)));
      if (curAgent.runtime) left.appendChild(el("span", "badge badge-neutral", "cli: " + curAgent.runtime));
      if (curAgent.location && curAgent.location.kind) {
        left.appendChild(el("span", "badge badge-neutral", "location: " + curAgent.location.kind));
      }
      top.appendChild(left);

      const agentPages = state.pages.filter(function (p) { return p.owner === curAgent.id && p.active; });
      if (agentPages.length > 0) {
        const pagesWrap = el("div", "header-actions");
        agentPages.forEach(function (pg) {
          pagesWrap.appendChild(buildPageChip(pg));
        });
        top.appendChild(pagesWrap);
      }
      headerBar.appendChild(top);
    }

    // Inline secret requests for the active session's agent
    inlineSecrets.innerHTML = "";
    const agSecrets = state.secrets.filter(function (s) {
      return s.agent === state.selectedAgent && s.state === "pending";
    });
    agSecrets.forEach(function (sec) {
      inlineSecrets.appendChild(buildSecretRequestCard(sec, true));
    });

    // Inline ACP permissions for the active session's agent
    inlinePerms.innerHTML = "";
    const agPerms = state.permissions.filter(function (p) {
      return p.agent === state.selectedAgent && p.state === "pending";
    });
    agPerms.forEach(function (perm) {
      inlinePerms.appendChild(buildPermissionCard(perm));
    });

    renderTranscriptDom();
  }

  function renderTranscriptDom() {
    const log = document.getElementById("session-transcript-log");
    if (!log) return;
    log.innerHTML = "";

    if (state.transcriptEvents.length === 0) {
      log.appendChild(el("div", "muted-text", "No ACP transcript messages yet for \"" + state.selectedAgent + "\". Send a prompt below to start a turn."));
      return;
    }

    state.transcriptEvents.forEach(function (raw) {
      const msg = (raw && raw.message) ? raw.message : raw;
      if (!msg) return;

      const method = msg.method || "";
      const params = msg.params || {};
      const update = params.update || {};
      const sessionUpdate = update.sessionUpdate || "";

      if (method === "session/prompt") {
        const item = el("div", "tx-item tx-user");
        const hdr = el("div", "tx-item-header");
        hdr.appendChild(el("span", "bold", "Human Prompt (session/prompt)"));
        item.appendChild(hdr);
        let text = params.text || "";
        if (!text && Array.isArray(params.prompt)) {
          text = params.prompt.map(function (b) { return b.text || ""; }).join("\n");
        }
        item.appendChild(el("div", "tx-item-body", text));
        log.appendChild(item);
        return;
      }

      if (sessionUpdate === "agent_thought_chunk" || update.kind === "thought") {
        const item = el("div", "tx-item tx-thought");
        item.appendChild(el("div", "tx-item-header", "Agent Thought"));
        const content = (update.content && update.content.text) || update.text || "";
        item.appendChild(el("div", "tx-item-body", content));
        log.appendChild(item);
        return;
      }

      if (sessionUpdate === "agent_message_chunk") {
        const item = el("div", "tx-item");
        item.appendChild(el("div", "tx-item-header", "Agent Message"));
        const content = (update.content && update.content.text) || update.text || "";
        item.appendChild(el("div", "tx-item-body", content));
        log.appendChild(item);
        return;
      }

      if (sessionUpdate === "tool_call" || sessionUpdate === "tool_call_update" || method === "session/request_permission") {
        const item = el("div", "tx-item tx-tool");
        const title = update.title || params.summary || method || sessionUpdate;
        item.appendChild(el("div", "tx-item-header", "Tool / Permission: " + title));
        if (update.diff || params.diff) {
          item.appendChild(renderUnifiedDiffBlock(update.diff || params.diff));
        } else {
          item.appendChild(el("div", "tx-item-body mono", JSON.stringify(update.content || params, null, 2)));
        }
        log.appendChild(item);
        return;
      }

      if (sessionUpdate === "plan" || update.entries) {
        const item = el("div", "tx-item");
        item.appendChild(el("div", "tx-item-header", "Agent Plan"));
        const entries = update.entries || [];
        entries.forEach(function (e) {
          item.appendChild(el("div", "tx-item-body mono", "[" + (e.status || "pending") + "] " + (e.content || e.title || "")));
        });
        log.appendChild(item);
        return;
      }

      if (msg.result && msg.result.stopReason) {
        const item = el("div", "tx-item");
        item.appendChild(el("div", "tx-item-header", "Turn Completed: stopReason=" + msg.result.stopReason));
        log.appendChild(item);
        return;
      }

      const item = el("div", "tx-item");
      item.appendChild(el("div", "tx-item-header", method || "ACP Frame"));
      item.appendChild(el("div", "tx-item-body mono", JSON.stringify(msg)));
      log.appendChild(item);
    });

    log.scrollTop = log.scrollHeight;
  }

  // ---- SECRET REQUEST BOX BUILDER (reused in Session View and Approvals Queue) ----
  function buildSecretRequestCard(sec, isInline) {
    const card = el("div", "secret-card");
    card.setAttribute("data-secret-id", sec.id);

    const top = el("div", "agent-node-top");
    const left = el("div", "agent-node-title");
    left.appendChild(el("span", "badge badge-warning", isInline ? "Inline Secret Request" : "Secret Request"));
    left.appendChild(el("span", "mono bold", sec.name));
    left.appendChild(el("span", "mono muted-text", "from agent: " + sec.agent));
    left.appendChild(el("span", "mono badge badge-neutral", "target path: " + sec.path));
    top.appendChild(left);
    card.appendChild(top);

    const reasonBox = el("div", "untrusted-reason-box");
    reasonBox.appendChild(el("div", "untrusted-reason-label", "Agent's reason for requesting secret (untrusted)"));
    reasonBox.appendChild(el("pre", "untrusted-reason-text", sec.reason || ""));
    card.appendChild(reasonBox);

    const notice = el("div", "muted-text small");
    notice.style.marginTop = "8px";
    notice.textContent = "Value is written directly to " + sec.path + " (mode 0600) in " + sec.agent + "'s environment via silo connect and is never echoed or stored in transcripts, state, or audit logs.";
    card.appendChild(notice);

    const inputRow = el("div", "secret-input-row");
    const singleInput = el("input", "input-field mono");
    singleInput.setAttribute("type", "password"); // type="password"
    singleInput.placeholder = "Enter secret value for " + sec.name + " (masked)…";
    singleInput.style.flex = "1";
    singleInput.autocomplete = "off";

    const multiInput = el("textarea", "input-field mono masked-textarea hidden");
    multiInput.rows = 3;
    multiInput.placeholder = "Enter multi-line secret value for " + sec.name + "…";
    multiInput.style.flex = "1";

    let isMulti = false;
    const toggleMultiBtn = el("button", "btn btn-sm btn-secondary", "Multi-line");
    toggleMultiBtn.type = "button";
    toggleMultiBtn.addEventListener("click", function () {
      isMulti = !isMulti;
      if (isMulti) {
        multiInput.value = singleInput.value;
        singleInput.classList.add("hidden");
        multiInput.classList.remove("hidden");
        toggleMultiBtn.textContent = "Single-line";
      } else {
        singleInput.value = multiInput.value;
        multiInput.classList.add("hidden");
        singleInput.classList.remove("hidden");
        toggleMultiBtn.textContent = "Multi-line";
      }
    });

    const provideBtn = el("button", "btn btn-sm btn-primary", "Provide Secret");
    provideBtn.type = "button";
    provideBtn.addEventListener("click", async function () {
      const val = isMulti ? multiInput.value : singleInput.value;
      if (!val) {
        showToast("Secret value cannot be empty", true);
        return;
      }
      // Clear inputs immediately so the value never lingers in the DOM
      singleInput.value = "";
      multiInput.value = "";
      try {
        await apiFetch("/api/v1/secrets/" + encodeURIComponent(sec.id) + "/provide", {
          method: "POST",
          body: { value: val }
        });
        showToast("Secret " + sec.name + " written directly to " + sec.path);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    });

    const noteInput = el("input", "input-field");
    noteInput.type = "text";
    noteInput.placeholder = "Rejection note (optional)…";
    noteInput.style.width = "200px";

    const rejectBtn = el("button", "btn btn-sm btn-danger", "Reject");
    rejectBtn.type = "button";
    rejectBtn.addEventListener("click", async function () {
      singleInput.value = "";
      multiInput.value = "";
      try {
        await apiFetch("/api/v1/secrets/" + encodeURIComponent(sec.id) + "/reject", {
          method: "POST",
          body: { note: noteInput.value }
        });
        showToast("Rejected secret request " + sec.name);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    });

    inputRow.appendChild(singleInput);
    inputRow.appendChild(multiInput);
    inputRow.appendChild(toggleMultiBtn);
    inputRow.appendChild(provideBtn);
    inputRow.appendChild(noteInput);
    inputRow.appendChild(rejectBtn);
    card.appendChild(inputRow);

    return card;
  }

  function buildPermissionCard(perm) {
    const card = el("div", "card");
    card.style.borderLeft = "4px solid var(--info)";
    const top = el("div", "agent-node-top");
    const left = el("div", "agent-node-title");
    left.appendChild(el("span", "badge badge-info", perm.kind === "register_agent" ? "Child Agent Registration" : "ACP Tool Permission"));
    left.appendChild(el("span", "mono bold", "agent: " + perm.agent));
    if (perm.summary) left.appendChild(el("span", "mono", perm.summary));
    top.appendChild(left);

    const actions = el("div", "header-actions");
    const allowBtn = el("button", "btn btn-sm btn-primary", "Allow");
    allowBtn.addEventListener("click", async function () {
      try {
        await apiFetch("/api/v1/permissions/" + encodeURIComponent(perm.id) + "/decision", {
          method: "POST",
          body: { decision: "approve" }
        });
        showToast("Approved permission " + perm.id);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    });

    const denyBtn = el("button", "btn btn-sm btn-danger", "Deny");
    denyBtn.addEventListener("click", async function () {
      try {
        await apiFetch("/api/v1/permissions/" + encodeURIComponent(perm.id) + "/decision", {
          method: "POST",
          body: { decision: "reject" }
        });
        showToast("Denied permission " + perm.id);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    });

    actions.appendChild(allowBtn);
    actions.appendChild(denyBtn);
    top.appendChild(actions);
    card.appendChild(top);

    if (perm.params) {
      const pre = el("pre", "code-box mono", typeof perm.params === "string" ? perm.params : JSON.stringify(perm.params, null, 2));
      card.appendChild(pre);
    }
    return card;
  }

  function renderUnifiedDiffBlock(diffText) {
    const box = el("div", "diff-block");
    const lines = String(diffText || "").split("\n");
    lines.forEach(function (line) {
      let cls = "diff-line";
      if (line.startsWith("+") && !line.startsWith("+++")) cls += " diff-add";
      else if (line.startsWith("-") && !line.startsWith("---")) cls += " diff-del";
      else if (line.startsWith("@@")) cls += " diff-hunk";
      box.appendChild(el("div", cls, line));
    });
    return box;
  }

  // ---- 4. APPROVALS & REQUESTS VIEW ----
  function renderApprovalsView() {
    // 4A. Pending run_as_root batches
    const pendBatchesList = document.getElementById("pending-batches-list");
    document.getElementById("pending-batches-count").textContent = String(state.batchesPending.length);
    pendBatchesList.innerHTML = "";

    if (state.batchesPending.length === 0) {
      pendBatchesList.appendChild(el("div", "card muted-text", "No pending run_as_root batches."));
    } else {
      state.batchesPending.forEach(function (summary) {
        const rec = state.batchDetails[summary.batch_id] || { id: summary.batch_id, status: summary.status, presentation: summary };
        pendBatchesList.appendChild(buildBatchApprovalCard(rec, true));
      });
    }

    // 4B. Pending secret requests
    const pendSecrets = state.secrets.filter(function (s) { return s.state === "pending"; });
    document.getElementById("pending-secrets-count").textContent = String(pendSecrets.length);
    const secretsList = document.getElementById("pending-secrets-list");
    secretsList.innerHTML = "";
    if (pendSecrets.length === 0) {
      secretsList.appendChild(el("div", "card muted-text", "No pending secret requests."));
    } else {
      pendSecrets.forEach(function (sec) {
        secretsList.appendChild(buildSecretRequestCard(sec, false));
      });
    }

    // 4C. Pending ACP & child registration permissions
    const pendPerms = state.permissions.filter(function (p) { return p.state === "pending"; });
    document.getElementById("pending-perms-count").textContent = String(pendPerms.length);
    const permsList = document.getElementById("pending-perms-list");
    permsList.innerHTML = "";
    if (pendPerms.length === 0) {
      permsList.appendChild(el("div", "card muted-text", "No pending ACP or child-registration permissions."));
    } else {
      pendPerms.forEach(function (perm) {
        permsList.appendChild(buildPermissionCard(perm));
      });
    }

    // 4D. General agent requests (file_request)
    const pendReqs = state.requests.filter(function (r) { return r.state === "pending"; });
    document.getElementById("pending-requests-count").textContent = String(pendReqs.length);
    const reqsList = document.getElementById("pending-requests-list");
    reqsList.innerHTML = "";
    if (pendReqs.length === 0) {
      reqsList.appendChild(el("div", "card muted-text", "No pending agent escalation requests."));
    } else {
      pendReqs.forEach(function (req) {
        reqsList.appendChild(buildAgentRequestCard(req));
      });
    }

    // 4E. Decided batches history with step outputs
    const decided = state.batchesAll.filter(function (b) { return b.status !== "pending"; });
    document.getElementById("decided-batches-count").textContent = String(decided.length);
    const decidedList = document.getElementById("decided-batches-list");
    decidedList.innerHTML = "";
    if (decided.length === 0) {
      decidedList.appendChild(el("div", "card muted-text", "No decided run_as_root batches yet."));
    } else {
      decided.forEach(function (summary) {
        const rec = state.batchDetails[summary.batch_id] || { id: summary.batch_id, status: summary.status, presentation: summary };
        decidedList.appendChild(buildBatchApprovalCard(rec, false));
      });
    }
  }

  function buildBatchApprovalCard(rec, isPending) {
    const p = rec.presentation || {};
    const batchID = rec.id || rec.batch_id || "";
    const shortHash = p.short_hash || rec.short_hash || (rec.hash ? rec.hash.slice(0, 16) : "");
    const fullHash = p.hash || rec.hash || shortHash;

    const card = el("div", "batch-card" + (isPending ? " batch-pending" : ""));
    card.setAttribute("data-batch-id", batchID);

    // 1. Header: title, submitter, hash, runs-as, status
    const hdr = el("div", "batch-header");
    const hdrLeft = el("div", "");
    hdrLeft.appendChild(el("h3", "batch-title", p.title || (rec.bound && rec.bound.title) || batchID));
    const subMeta = el("div", "mono muted-text small");
    subMeta.textContent = "id: " + batchID + " · from: " + (p.submitted_by || rec.submitter || "root") + " · runs as: " + (p.runs_as || "root (uid 0)");
    hdrLeft.appendChild(subMeta);
    hdr.appendChild(hdrLeft);

    const hdrRight = el("div", "agent-node-title");
    const hashBadge = el("span", "badge badge-neutral mono", "hash: " + shortHash);
    hashBadge.title = "Full SHA-256: " + fullHash;
    hdrRight.appendChild(hashBadge);
    hdrRight.appendChild(el("span", "badge " + (rec.status === "succeeded" ? "badge-ok" : (rec.status === "pending" ? "badge-warning" : "badge-danger")), rec.status));
    hdr.appendChild(hdrRight);
    card.appendChild(hdr);

    // Snapshot banner (prominent red "no snapshot available — not undoable" when none)
    const snapAvailable = Boolean(p.snapshot_available);
    const snapText = p.snapshot || "no snapshot available — not undoable";
    const snapBanner = el("div", "snapshot-banner " + (snapAvailable ? "snapshot-ok" : "snapshot-none"));
    snapBanner.textContent = snapAvailable ? snapText : ("⚠ No snapshot available — not undoable (" + snapText + ")");
    card.appendChild(snapBanner);

    // 2. Computed Facts & Risk Flags (shown BEFORE the agent's untrusted explanation)
    const flags = Array.isArray(p.flags) ? p.flags : [];
    if (flags.length > 0) {
      const flagSec = el("div", "facts-section");
      flagSec.appendChild(el("h4", "", "Computed Risk Flags (" + flags.length + ")"));
      const flagList = el("div", "risk-flag-list");
      flags.forEach(function (fl) {
        const isBlocking = Boolean(fl.blocking) || fl.code === "root-instructions" || fl.message === "changes the root agent's own instructions";
        const row = el("div", "risk-flag-item" + (isBlocking ? " flag-blocking" : ""));
        row.appendChild(el("span", "badge " + (isBlocking ? "badge-danger" : "badge-warning"), fl.code));
        row.appendChild(el("span", "", fl.message || (fl.code === "root-instructions" ? "changes the root agent's own instructions" : "")));
        if (fl.where) {
          row.appendChild(el("span", "mono muted-text", "(" + fl.where + ")"));
        }
        flagList.appendChild(row);
      });
      flagSec.appendChild(flagList);
      card.appendChild(flagSec);
    }

    // Files written or deleted + Unified Diffs
    const files = Array.isArray(p.files) ? p.files : [];
    if (files.length > 0) {
      const fileSec = el("div", "facts-section");
      fileSec.appendChild(el("h4", "", "Staged File Writes & Unified Diffs (" + files.length + ")"));
      files.forEach(function (f, idx) {
        const fBox = el("div", "argv-row");
        const action = f.delete ? "DELETE" : (f.exists ? "WRITE" : "CREATE");
        const fHdr = el("div", "mono bold");
        fHdr.textContent = "files[" + idx + "] " + action + " " + f.path + (f.delete ? "" : (" (" + (f.mode || "0644") + " " + (f.owner || "root") + ":" + (f.group || "root") + ", " + (f.size || 0) + " bytes)"));
        fBox.appendChild(fHdr);
        if (f.defaults_diff) {
          fBox.appendChild(el("div", "muted-text small mono", "Diff against shipped defaults (root instructions):"));
          fBox.appendChild(renderUnifiedDiffBlock(f.defaults_diff));
        }
        if (f.diff) {
          fBox.appendChild(renderUnifiedDiffBlock(f.diff));
        }
        fileSec.appendChild(fBox);
      });
      card.appendChild(fileSec);
    }

    // Resolved argv per step (rendered element-by-element in boxes)
    const steps = Array.isArray(p.steps) ? p.steps : [];
    if (steps.length > 0) {
      const stepSec = el("div", "facts-section");
      stepSec.appendChild(el("h4", "", "Resolved Commands — Argv Element by Element (" + steps.length + ")"));
      steps.forEach(function (st) {
        const sRow = el("div", "argv-row");
        const topLine = el("div", "mono small muted-text");
        topLine.textContent = "steps[" + st.index + "]" + (st.op ? (" (" + st.op + ")") : "") + " · exe: " + (st.exe || "") + (st.exe_sha256 ? (" (sha256:" + st.exe_sha256.slice(0, 16) + ")") : "") + " · cwd: " + (st.cwd || "/");
        sRow.appendChild(topLine);

        const boxes = el("div", "argv-boxes");
        (st.argv || []).forEach(function (arg, idx) {
          const tok = el("span", "argv-token");
          tok.appendChild(el("span", "argv-token-idx", "[" + idx + "]"));
          tok.appendChild(el("span", "", arg));
          boxes.appendChild(tok);
        });
        sRow.appendChild(boxes);
        stepSec.appendChild(sRow);
      });
      card.appendChild(stepSec);
    }

    // 3. Agent's untrusted explanation (always rendered AFTER computed facts & diffs, as plain text)
    if (p.reason || (rec.bound && rec.bound.reason)) {
      const reasonBox = el("div", "untrusted-reason-box");
      reasonBox.appendChild(el("div", "untrusted-reason-label", p.reason_label || "Agent's explanation (untrusted)"));
      reasonBox.appendChild(el("pre", "untrusted-reason-text", p.reason || (rec.bound && rec.bound.reason) || ""));
      card.appendChild(reasonBox);
    }

    // 4. If Pending: Approve / Reject with comment (and short-hash confirmation if root-instructions flagged)
    if (isPending) {
      const bar = el("div", "decision-bar");
      const commentInput = el("input", "input-field");
      commentInput.type = "text";
      commentInput.placeholder = "Decision comment (required to reject, optional to approve)…";
      commentInput.style.flex = "1";
      bar.appendChild(commentInput);

      let confirmInput = null;
      if (p.needs_hash_confirmation) {
        confirmInput = el("input", "input-field mono");
        confirmInput.type = "text";
        confirmInput.placeholder = "Type short hash " + shortHash + " to confirm root instruction edit";
        confirmInput.style.width = "300px";
        bar.appendChild(confirmInput);
      }

      const approveBtn = el("button", "btn btn-primary", "Approve & Run as Root");
      approveBtn.addEventListener("click", async function () {
        const body = {
          decision: "approve",
          comment: commentInput.value,
          confirm: confirmInput ? confirmInput.value.trim() : ""
        };
        try {
          await apiFetch("/api/v1/approvals/" + encodeURIComponent(batchID) + "/decision", {
            method: "POST",
            body: body
          });
          showToast("Approved batch " + batchID);
          await refreshAll();
        } catch (err) {
          showToast(err.message, true);
        }
      });

      const rejectBtn = el("button", "btn btn-danger", "Reject Batch");
      rejectBtn.addEventListener("click", async function () {
        if (!commentInput.value.trim()) {
          showToast("Reject requires a comment explaining why", true);
          commentInput.focus();
          return;
        }
        try {
          await apiFetch("/api/v1/approvals/" + encodeURIComponent(batchID) + "/decision", {
            method: "POST",
            body: {
              decision: "reject",
              comment: commentInput.value.trim()
            }
          });
          showToast("Rejected batch " + batchID);
          await refreshAll();
        } catch (err) {
          showToast(err.message, true);
        }
      });

      bar.appendChild(approveBtn);
      bar.appendChild(rejectBtn);
      card.appendChild(bar);
    } else {
      // Decided batch: show decision info and step outputs
      if (rec.decision) {
        const decLine = el("div", "facts-section mono small muted-text");
        decLine.textContent = "Decision: " + rec.decision.decision + " by " + rec.decision.by + (rec.decision.comment ? (" — \"" + rec.decision.comment + "\"") : "");
        card.appendChild(decLine);
      }
      if (rec.error) {
        card.appendChild(el("div", "error-banner mono", rec.error));
      }
      const stepResults = Array.isArray(rec.steps) ? rec.steps : [];
      if (stepResults.length > 0) {
        const outSec = el("div", "facts-section");
        outSec.appendChild(el("h4", "", "Executed Step Outputs (" + stepResults.length + ")"));
        stepResults.forEach(function (sr) {
          const srBox = el("div", "argv-row mono");
          srBox.appendChild(el("div", "bold", "step[" + sr.index + "] " + JSON.stringify(sr.argv || []) + " → exit " + sr.exit + " (" + (sr.duration_s || 0) + "s)"));
          if (sr.stdout) {
            srBox.appendChild(el("pre", "code-box", "stdout:\n" + sr.stdout));
          }
          if (sr.stderr) {
            srBox.appendChild(el("pre", "code-box", "stderr:\n" + sr.stderr));
          }
          outSec.appendChild(srBox);
        });
        card.appendChild(outSec);
      }
    }

    return card;
  }

  function buildAgentRequestCard(req) {
    const card = el("div", "card");
    const top = el("div", "agent-node-top");
    const left = el("div", "agent-node-title");
    left.appendChild(el("span", "badge badge-neutral", req.kind || "other"));
    left.appendChild(el("span", "mono bold", "from: " + req.agent));
    left.appendChild(el("span", "bold", req.summary));
    top.appendChild(left);
    card.appendChild(top);

    if (req.detail) {
      const detailBox = el("div", "untrusted-reason-box");
      detailBox.appendChild(el("div", "untrusted-reason-label", "Request detail from agent " + req.agent + " (untrusted content)"));
      detailBox.appendChild(el("pre", "untrusted-reason-text", req.detail));
      card.appendChild(detailBox);
    }

    const bar = el("div", "decision-bar");
    const noteInput = el("input", "input-field");
    noteInput.type = "text";
    noteInput.placeholder = "Note for agent or root…";
    noteInput.style.flex = "1";
    bar.appendChild(noteInput);

    async function resolveWith(resolution, label) {
      try {
        await apiFetch("/api/v1/requests/" + encodeURIComponent(req.id) + "/resolve", {
          method: "POST",
          body: { resolution: resolution, note: noteInput.value }
        });
        showToast(label + " request " + req.id);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    }

    const resBtn = el("button", "btn btn-sm btn-primary", "Resolve");
    resBtn.addEventListener("click", function () { resolveWith("resolved", "Resolved"); });

    const fwdBtn = el("button", "btn btn-sm btn-secondary", "Forward to Root (Marked Untrusted)");
    fwdBtn.addEventListener("click", function () { resolveWith("forwarded", "Forwarded to root:"); });

    const disBtn = el("button", "btn btn-sm btn-danger", "Dismiss");
    disBtn.addEventListener("click", function () { resolveWith("rejected", "Dismissed"); });

    bar.appendChild(resBtn);
    bar.appendChild(fwdBtn);
    bar.appendChild(disBtn);
    card.appendChild(bar);
    return card;
  }

  // ---- 5. AUDIT LOG VIEW ----
  function renderAuditView() {
    const banner = document.getElementById("audit-verify-banner");
    const tbody = document.getElementById("audit-tbody");
    const filterVal = (document.getElementById("audit-filter-input")?.value || "").toLowerCase().trim();
    if (!banner || !tbody) return;

    const v = state.auditVerify || { ok: true, entries: state.auditEntries.length };
    if (v.ok !== false) {
      banner.className = "verify-banner verify-ok";
      banner.textContent = "✔ Audit Hash Chain Verified (" + (v.entries || state.auditEntries.length) + " entries intact)";
    } else {
      banner.className = "verify-banner verify-bad";
      banner.textContent = "✘ Audit Hash Chain Verification FAILED at seq " + v.first_bad_seq + ": " + (v.reason || "tampered");
    }

    tbody.innerHTML = "";
    const filtered = state.auditEntries.filter(function (e) {
      if (!filterVal) return true;
      const hay = (e.actor + " " + e.event + " " + JSON.stringify(e.payload || {})).toLowerCase();
      return hay.includes(filterVal);
    });

    filtered.slice().reverse().forEach(function (e) {
      const tr = el("tr", "");
      tr.appendChild(el("td", "mono", e.seq));
      tr.appendChild(el("td", "mono muted-text", e.time));
      tr.appendChild(el("td", "mono bold", e.actor));
      tr.appendChild(el("td", "mono", e.event));
      tr.appendChild(el("td", "mono", JSON.stringify(e.payload || {})));
      tr.appendChild(el("td", "mono muted-text", e.prev ? e.prev.slice(0, 16) + "…" : "(genesis)"));
      tbody.appendChild(tr);
    });
  }

  function switchView(viewName) {
    state.activeView = viewName;
    document.querySelectorAll(".nav-tab").forEach(function (btn) {
      btn.classList.toggle("active", btn.getAttribute("data-view") === viewName);
    });
    ["agents", "pages", "session", "approvals", "audit"].forEach(function (v) {
      const sec = document.getElementById("view-" + v);
      if (sec) sec.classList.toggle("hidden", v !== viewName);
    });
    if (viewName === "session") {
      connectSessionStream(state.selectedAgent || "main");
    }
  }

  // ---- WIRE DOM EVENTS ----
  document.addEventListener("DOMContentLoaded", async function () {
    document.querySelectorAll(".nav-tab").forEach(function (btn) {
      btn.addEventListener("click", function () {
        switchView(btn.getAttribute("data-view"));
      });
    });

    document.getElementById("refresh-all-btn")?.addEventListener("click", function () {
      refreshAll();
    });

    // Login form
    document.getElementById("login-form")?.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      const errBanner = document.getElementById("login-error");
      errBanner.classList.add("hidden");
      const tok = document.getElementById("login-token-input").value.trim();
      try {
        const res = await apiFetch("/api/v1/auth/login", {
          method: "POST",
          body: { token: tok }
        });
        state.authenticated = true;
        state.csrfToken = (res && res.csrf_token) || "";
        state.subject = (res && res.subject) || "admin";
        document.getElementById("login-token-input").value = "";
        showLoginModal(false);
        await bootstrapAuth();
        await refreshAll();
      } catch (err) {
        errBanner.textContent = err.message || "Invalid token";
        errBanner.classList.remove("hidden");
      }
    });

    document.getElementById("logout-btn")?.addEventListener("click", async function () {
      try {
        await apiFetch("/api/v1/auth/logout", { method: "POST" });
      } catch (_) {}
      state.authenticated = false;
      state.csrfToken = "";
      showLoginModal(true);
    });

    // Register Agent Drawer
    document.getElementById("toggle-register-agent-btn")?.addEventListener("click", function () {
      document.getElementById("register-agent-panel").classList.toggle("hidden");
    });
    document.getElementById("cancel-register-agent-btn")?.addEventListener("click", function () {
      document.getElementById("register-agent-panel").classList.add("hidden");
    });
    document.getElementById("register-agent-form")?.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      const name = document.getElementById("reg-agent-name").value.trim();
      const parent = document.getElementById("reg-agent-parent").value;
      const locKind = document.getElementById("reg-agent-location-kind").value;
      const locDetail = document.getElementById("reg-agent-location-detail").value.trim();
      const runtime = document.getElementById("reg-agent-runtime").value;
      const user = document.getElementById("reg-agent-user").value.trim();
      const permissions = document.getElementById("reg-agent-permissions").value;
      const mayRegister = document.getElementById("reg-agent-may-register").checked;
      const brief = document.getElementById("reg-agent-brief").value.trim();

      try {
        const res = await apiFetch("/api/v1/agents", {
          method: "POST",
          body: {
            name: name,
            parent: parent,
            user: user,
            location: { kind: locKind, detail: locDetail },
            runtime: runtime,
            permissions: permissions,
            may_register: mayRegister,
            brief: brief
          }
        });
        const box = document.getElementById("minted-token-box");
        document.getElementById("minted-agent-id").textContent = "Agent: " + res.agent + " (" + res.state + ")";
        document.getElementById("minted-token-value").textContent = res.token;
        const wsHost = (window.location.protocol === "https:" ? "wss://" : "ws://") + window.location.host + "/connect";
        const cliCmd = runtime === "gemini" ? "gemini --experimental-acp" : (runtime === "codex" ? "codex-acp" : "claude-code-acp");
        document.getElementById("minted-connect-cmd").textContent =
          "# On this host (Unix socket):\n" +
          "SILO_AGENT_TOKEN=" + res.token + " silo connect -- " + cliCmd + "\n\n" +
          "# From container / microVM / remote over WebSocket:\n" +
          "SILO_AGENT_TOKEN=" + res.token + " silo connect --url " + wsHost + " -- " + cliCmd;
        box.classList.remove("hidden");
        showToast("Registered agent " + res.agent);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    });

    // Stop Agent Modal
    document.getElementById("stop-cancel-btn")?.addEventListener("click", function () {
      document.getElementById("stop-modal").classList.add("hidden");
    });
    document.getElementById("stop-confirm-btn")?.addEventListener("click", async function () {
      const target = state.stopTargetAgent;
      const mode = document.getElementById("stop-mode-select").value;
      document.getElementById("stop-modal").classList.add("hidden");
      if (!target) return;
      try {
        const res = await apiFetch("/api/v1/agents/" + encodeURIComponent(target) + "/stop", {
          method: "POST",
          body: { mode: mode }
        });
        showToast("Stopped: " + ((res && res.stopped) ? res.stopped.join(", ") : target));
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    });

    // Register Page Drawer
    document.getElementById("toggle-register-page-btn")?.addEventListener("click", function () {
      document.getElementById("register-page-panel").classList.toggle("hidden");
    });
    document.getElementById("cancel-register-page-btn")?.addEventListener("click", function () {
      document.getElementById("register-page-panel").classList.add("hidden");
    });
    document.getElementById("page-kind-select")?.addEventListener("change", function (ev) {
      const isExt = ev.target.value === "external";
      document.getElementById("page-internal-fields").classList.toggle("hidden", isExt);
      document.getElementById("page-external-fields").classList.toggle("hidden", !isExt);
    });
    document.getElementById("pages-show-inactive")?.addEventListener("change", function () {
      refreshAll();
    });
    document.getElementById("register-page-form")?.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      const slug = document.getElementById("page-slug-input").value.trim();
      const title = document.getElementById("page-title-input").value.trim();
      const kind = document.getElementById("page-kind-select").value;
      const body = { slug: slug, title: title };
      if (kind === "external") {
        body.url = document.getElementById("page-url-input").value.trim();
      } else {
        body.port = parseInt(document.getElementById("page-port-input").value, 10) || 0;
        body.host = document.getElementById("page-host-input").value.trim() || "127.0.0.1";
        const exp = parseInt(document.getElementById("page-expose-input").value, 10);
        if (exp > 0) body.expose_port = exp;
      }
      try {
        await apiFetch("/api/v1/pages", { method: "POST", body: body });
        document.getElementById("register-page-panel").classList.add("hidden");
        showToast("Registered page link " + slug);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    });

    // Session prompt form
    document.getElementById("session-prompt-form")?.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      const inp = document.getElementById("session-prompt-input");
      const text = inp.value.trim();
      if (!text || !state.selectedAgent) return;
      inp.value = "";
      try {
        await apiFetch("/api/v1/sessions/" + encodeURIComponent(state.selectedAgent) + "/prompt", {
          method: "POST",
          body: { text: text }
        });
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    });
    document.getElementById("session-prompt-input")?.addEventListener("keydown", function (ev) {
      if (ev.key === "Enter" && !ev.shiftKey) {
        ev.preventDefault();
        document.getElementById("session-prompt-form").requestSubmit();
      }
    });
    document.getElementById("session-cancel-btn")?.addEventListener("click", async function () {
      if (!state.selectedAgent) return;
      try {
        await apiFetch("/api/v1/sessions/" + encodeURIComponent(state.selectedAgent) + "/cancel", {
          method: "POST"
        });
        showToast("Cancelled active turn on " + state.selectedAgent);
      } catch (err) {
        showToast(err.message, true);
      }
    });

    // Audit filter & verify
    document.getElementById("audit-filter-input")?.addEventListener("input", function () {
      renderAuditView();
    });
    document.getElementById("verify-audit-btn")?.addEventListener("click", async function () {
      try {
        state.auditVerify = await apiFetch("/api/v1/audit/verify");
        renderAuditView();
        showToast(state.auditVerify.ok ? "Audit chain verified OK" : "Audit chain verification failed!", !state.auditVerify.ok);
      } catch (err) {
        showToast(err.message, true);
      }
    });

    const ok = await bootstrapAuth();
    if (ok) {
      await refreshAll();
    }
    function applyRouteFromURL() {
      const params = new URLSearchParams(window.location.search);
      const agParam = params.get("agent");
      if (agParam) {
        state.selectedAgent = agParam;
      }
      const viewParam = params.get("view") || window.location.hash.replace(/^#/, "");
      if (["agents", "pages", "session", "approvals", "audit"].includes(viewParam)) {
        switchView(viewParam);
        renderSessionView();
      }
    }
    applyRouteFromURL();
    window.addEventListener("hashchange", applyRouteFromURL);
    state.pollTimer = setInterval(function () {
      if (state.authenticated) refreshAll();
    }, 3000);
  });
})();
