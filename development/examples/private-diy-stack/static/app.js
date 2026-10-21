(function () {
  "use strict";

  const elNodeCards = document.getElementById("node-cards-container");
  const elCatalogOut = document.getElementById("act1-catalog-output");
  const elAct2Steps = document.getElementById("act2-steps-container");
  const elTaskInput = document.getElementById("input-agent-task");
  const elAuditBody = document.getElementById("audit-table-body");
  const elMetricInvoices = document.getElementById("metric-invoices");
  const elMetricWires = document.getElementById("metric-wires");
  const elMetricBan = document.getElementById("metric-ban-status");
  const elMeshStatusText = document.getElementById("mesh-status-text");

  function highlightTopology(mode) {
    const boxes = ["box-cp", "box-agent", "box-gpu", "box-vault"];
    const links = ["link-cp-agent", "link-cp-gpu", "link-cp-vault", "link-agent-gpu", "link-agent-vault"];
    boxes.forEach((id) => {
      const el = document.getElementById(id);
      if (el) el.classList.remove("active-node", "denied-node");
    });
    links.forEach((id) => {
      const el = document.getElementById(id);
      if (el) el.classList.remove("link-active", "link-deny");
    });

    if (mode === "discover" || mode === "react") {
      document.getElementById("box-agent").classList.add("active-node");
      document.getElementById("box-gpu").classList.add("active-node");
      document.getElementById("box-vault").classList.add("active-node");
      document.getElementById("link-agent-gpu").classList.add("link-active");
      document.getElementById("link-agent-vault").classList.add("link-active");
    } else if (mode === "deny-vault") {
      document.getElementById("box-agent").classList.add("active-node");
      document.getElementById("box-vault").classList.add("denied-node");
      document.getElementById("link-agent-vault").classList.add("link-deny");
    } else if (mode === "ban") {
      document.getElementById("box-cp").classList.add("denied-node");
    }
  }

  function escapeHtml(str) {
    return String(str || "")
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;");
  }

  async function refreshState() {
    try {
      const res = await fetch("/api/state");
      const data = await res.json();

      if (data.nodes && elNodeCards) {
        elNodeCards.innerHTML = data.nodes
          .map(
            (n) => `
          <div class="node-mini-card">
            <div class="node-mini-header">
              <span>${escapeHtml(n.title)}</span>
              <span class="${n.banned ? "badge-deny" : "badge-allow"}">${n.banned ? "BANNED" : "ONLINE"}</span>
            </div>
            <div class="peer-id-mono" title="${escapeHtml(n.peer_id)}">${escapeHtml(n.short_peer)}</div>
            <div class="label-chips">
              ${(n.labels || []).map((l) => `<span class="chip">${escapeHtml(l)}</span>`).join("")}
            </div>
          </div>
        `
          )
          .join("");
      }

      if (data.sqlite) {
        elMetricInvoices.textContent = `${data.sqlite.invoices_count} rows ($48,500 pending)`;
        elMetricWires.textContent = `${data.sqlite.wire_transfers_count} rows (Untouched)`;
      }
      elMetricBan.textContent = data.external_banned ? "BANNED (Revoked)" : "Enrolled (dept=external)";
      elMeshStatusText.textContent = data.external_banned
        ? "Mesh Online · 3 Active Sidecars · 1 Peer Banned"
        : "Mesh Online · 4 Sidecars Enrolled";

      if (Array.isArray(data.audit) && elAuditBody) {
        if (data.audit.length === 0) {
          elAuditBody.innerHTML = `<tr><td colspan="4">No sidecar audit events recorded yet. Run an Act to generate traffic.</td></tr>`;
        } else {
          elAuditBody.innerHTML = data.audit
            .slice()
            .reverse()
            .map(
              (ev) => `
            <tr>
              <td><span class="${ev.decision === "ALLOW" ? "badge-allow" : "badge-deny"}">${escapeHtml(ev.decision)}</span></td>
              <td>${escapeHtml(ev.caller)}</td>
              <td>${escapeHtml(ev.method)}</td>
              <td>${escapeHtml(ev.service)}</td>
            </tr>
          `
            )
            .join("");
        }
      }
    } catch (err) {
      console.error("Failed to refresh state:", err);
    }
  }

  async function runAct1() {
    const btn = document.getElementById("btn-act1-discover");
    btn.disabled = true;
    highlightTopology("discover");
    elCatalogOut.textContent = "Querying local sidecar http://127.0.0.1:19101/mcp (find_remote_tools) & http://127.0.0.1:19101/v1/models...";
    try {
      const res = await fetch("/api/act1/discover", { method: "POST" });
      const data = await res.json();
      elCatalogOut.textContent = data.summary || JSON.stringify(data, null, 2);
      await refreshState();
    } finally {
      btn.disabled = false;
    }
  }

  async function runAct2() {
    const btn = document.getElementById("btn-act2-run");
    btn.disabled = true;
    highlightTopology("react");
    elAct2Steps.innerHTML = `
      <div class="step-card">
        <div class="step-card-header"><span>Executing agent_harness.py against 127.0.0.1:19101...</span></div>
      </div>
    `;
    try {
      const res = await fetch("/api/act2/run-agent", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ task: elTaskInput.value }),
      });
      const data = await res.json();
      elAct2Steps.innerHTML = `
        <div class="step-card">
          <div class="step-card-header">
            <span>Step 1 · Local MCP Discovery (<code>http://127.0.0.1:19101/mcp</code>)</span>
            <span class="badge-allow">ALLOW</span>
          </div>
          <pre class="code-output">${escapeHtml(data.step1_discovery)}</pre>
        </div>
        <div class="step-card">
          <div class="step-card-header">
            <span>Step 2 · Self-Hosted Reasoning Plan (<code>inference://private-llm</code> → <code>deepseek-r1:1.5b</code>)</span>
            <span class="badge-allow">ALLOW</span>
          </div>
          <div class="think-box">&lt;think&gt;\n${escapeHtml(data.step2_think)}\n&lt;/think&gt;</div>
          <pre class="code-output">${escapeHtml(data.step2_plan)}</pre>
        </div>
        <div class="step-card">
          <div class="step-card-header">
            <span>Step 3 · Mesh Tool &amp; Internal API Execution (<code>mcp://erp-db</code> + <code>egress://compliance.internal</code>)</span>
            <span class="badge-allow">ALLOW + SECRET INJECTED</span>
          </div>
          <pre class="code-output">${escapeHtml(data.step3_results)}</pre>
        </div>
        <div class="step-card">
          <div class="step-card-header">
            <span>Step 4 · Final Audit Synthesis (<code>deepseek-r1:1.5b</code>)</span>
            <span class="badge-allow">VERIFIED</span>
          </div>
          <div class="think-box">&lt;think&gt;\n${escapeHtml(data.step4_think)}\n&lt;/think&gt;</div>
          <div class="decision-banner">${escapeHtml(data.step4_decision)}</div>
        </div>
      `;
      await refreshState();
    } finally {
      btn.disabled = false;
    }
  }

  async function runGuardrailCheck(checkId) {
    const resultEl = document.getElementById(`result-${checkId}`);
    if (resultEl) {
      resultEl.textContent = "Testing policy guardrail against live Vault sidecar...";
    }
    highlightTopology("deny-vault");
    const res = await fetch("/api/act3/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ check: checkId }),
    });
    const data = await res.json();
    if (resultEl) {
      resultEl.textContent = data.output;
      resultEl.classList.add("is-denied");
    }
    await refreshState();
  }

  async function runAct4Ban() {
    const btn = document.getElementById("btn-act4-ban");
    btn.disabled = true;
    highlightTopology("ban");
    try {
      await fetch("/api/act4/ban", { method: "POST" });
      await refreshState();
    } finally {
      btn.disabled = false;
    }
  }

  async function runAllActsSequentially() {
    const btn = document.getElementById("btn-run-all-acts");
    btn.disabled = true;
    try {
      await runAct1();
      await runAct2();
      await runGuardrailCheck("3a");
      await runGuardrailCheck("3b");
      await runGuardrailCheck("3c");
      await runGuardrailCheck("3d");
      await runAct4Ban();
    } finally {
      btn.disabled = false;
    }
  }

  document.getElementById("btn-act1-discover").addEventListener("click", runAct1);
  document.getElementById("btn-act2-run").addEventListener("click", runAct2);
  document.getElementById("btn-test-3a").addEventListener("click", () => runGuardrailCheck("3a"));
  document.getElementById("btn-test-3b").addEventListener("click", () => runGuardrailCheck("3b"));
  document.getElementById("btn-test-3c").addEventListener("click", () => runGuardrailCheck("3c"));
  document.getElementById("btn-test-3d").addEventListener("click", () => runGuardrailCheck("3d"));
  document.getElementById("btn-act3-run-all").addEventListener("click", async () => {
    await runGuardrailCheck("3a");
    await runGuardrailCheck("3b");
    await runGuardrailCheck("3c");
    await runGuardrailCheck("3d");
  });
  document.getElementById("btn-act4-ban").addEventListener("click", runAct4Ban);
  document.getElementById("btn-run-all-acts").addEventListener("click", runAllActsSequentially);
  document.getElementById("btn-refresh-state").addEventListener("click", refreshState);

  refreshState();
  setInterval(refreshState, 4000);
})();
