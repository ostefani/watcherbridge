// WatcherBridge viewer: live briefing feed over SSE. Every string from
// the server is rendered with textContent — never raw-HTML insertion —
// so hostile hostnames and prompt-injection banners stay inert text.
(function () {
  "use strict";
  var state = { briefings: [], groups: [], selected: null };
  var connEl = document.getElementById("conn");

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = text;
    return n;
  }

  function age(iso) {
    var s = Math.max(0, Math.floor((Date.now() - Date.parse(iso)) / 1000));
    if (s < 60) return s + "s ago";
    if (s < 3600) return Math.floor(s / 60) + "m ago";
    return Math.floor(s / 3600) + "h ago";
  }

  function matches(b) {
    var pr = document.getElementById("filter-priority").value;
    var ho = document.getElementById("filter-host").value.trim().toLowerCase();
    var tx = document.getElementById("filter-text").value.trim().toLowerCase();
    if (pr && b.priority !== pr) return false;
    if (ho && b.host.toLowerCase().indexOf(ho) < 0) return false;
    if (tx) {
      var hay = [b.host, b.priority, b.briefing_id, b.group_id,
        b.what_changed, b.context, b.priority_reason,
        b.recommended_check, (b.event_ids || []).join(" ")].join("\n").toLowerCase();
      if (hay.indexOf(tx) < 0) return false;
    }
    return true;
  }

  function explainer(b) {
    // Visible proof of single ownership, rendered from stored fields only.
    if (b.fallback_used) return "Fallback — priority = baseline, no model suggestion";
    var base = "Baseline: " + b.baseline.toUpperCase();
    var fin = "Final: " + b.priority.toUpperCase();
    if (!b.model_suggestion) return base + " —— model abstained ——> " + fin;
    var mid = "model suggested " + b.model_suggestion.toUpperCase();
    if (!b.clamped) return base + " ── " + mid + " ──> " + fin;
    return base + " ── " + mid + " ──> " + fin +
      "\nModel suggested " + b.model_suggestion.toUpperCase() +
      " — clamped to " + b.priority.toUpperCase();
  }

  function renderGroups() {
    var ul = document.getElementById("group-list");
    while (ul.firstChild) ul.removeChild(ul.firstChild);
    var now = Math.floor(Date.now() / 1000);
    state.groups.forEach(function (g) {
      var li = el("li", null, null);
      li.appendChild(el("span", "pill " + "medium", "OPEN"));
      li.appendChild(el("span", "meta", g.anchor + " · " + g.event_count + " events · closes in " +
        Math.max(0, g.deadline_unix - now) + "s"));
      var bar = el("div", "countbar", null);
      var fill = el("div", null, null);
      bar.appendChild(fill);
      li.appendChild(bar);
      li.setAttribute("data-deadline", String(g.deadline_unix));
      li.setAttribute("data-opened", String(now));
      ul.appendChild(li);
    });
    tickCountdowns();
  }

  function tickCountdowns() {
    var now = Math.floor(Date.now() / 1000);
    var items = document.getElementById("group-list").children;
    for (var i = 0; i < items.length; i++) {
      var li = items[i];
      var deadline = parseInt(li.getAttribute("data-deadline"), 10);
      var opened = parseInt(li.getAttribute("data-opened"), 10) || now;
      var total = Math.max(1, deadline - opened);
      var left = Math.max(0, deadline - now);
      var fill = li.querySelector(".countbar > div");
      if (fill) fill.style.width = (100 * left / total).toFixed(1) + "%";
    }
  }

  function renderBriefs() {
    var ol = document.getElementById("brief-list");
    while (ol.firstChild) ol.removeChild(ol.firstChild);
    state.briefings.filter(matches).forEach(function (b) {
      var li = el("li", null, null);
      li.setAttribute("aria-selected", b.briefing_id === state.selected ? "true" : "false");
      li.appendChild(el("span", "pill " + b.priority, b.priority.toUpperCase()));
      li.appendChild(el("strong", null, b.host));
      li.appendChild(el("div", "meta", age(b.created_at) + " · " + b.event_ids.length + " events"));
      li.appendChild(el("div", null, b.what_changed));
      (function (id) {
        li.addEventListener("click", function () { selectBriefing(id); });
      })(b.briefing_id);
      ol.appendChild(li);
    });
  }

  function selectBriefing(id) {
    state.selected = id;
    renderBriefs();
    var box = document.getElementById("dossier");
    while (box.firstChild) box.removeChild(box.firstChild);
    var b = null;
    state.briefings.forEach(function (x) { if (x.briefing_id === id) b = x; });
    if (!b) { box.appendChild(el("p", "hint", "Select a briefing.")); return; }
    var step = el("div", "stepper", explainer(b));
    box.appendChild(step);
    var dl = el("dl", "kv", null);
    [["Briefing", b.briefing_id], ["Group", b.group_id], ["Host", b.host],
     ["Baseline", b.baseline], ["Model suggestion", b.model_suggestion || "— (abstained)"],
     ["Final priority", b.priority + (b.clamped ? " (clamped)" : "")],
     ["Baseline rule", b.baseline_rule || "—"],
     ["Reason", b.priority_reason], ["What changed", b.what_changed],
     ["Context", b.context], ["Recommended check", b.recommended_check],
     ["Events", (b.event_ids || []).join(", ")],
     ["Created", b.created_at]
    ].forEach(function (kv) {
      dl.appendChild(el("dt", null, kv[0]));
      dl.appendChild(el("dd", null, kv[1]));
    });
    box.appendChild(dl);
  }

  function upsertBriefing(b) {
    var found = false;
    state.briefings = state.briefings.map(function (x) {
      if (x.briefing_id === b.briefing_id) { found = true; return b; }
      return x;
    });
    if (!found) state.briefings.unshift(b);
    renderBriefs();
    if (state.selected === b.briefing_id) selectBriefing(b.briefing_id);
  }

  var src = null;
  var streamGen = 0;
  var lastEventId = "";
  // pendingGap is a resync gap being paged in: {cursor, boundary, liveMax,
  // running, timer}. Live events upsert data but never move lastEventId
  // past the unrecovered range; the loop owns the cursor until the
  // boundary (the resync max_seq) is reached.
  var pendingGap = null;

  // runGapFill pages ?after= toward the resync boundary, a calm page at a
  // time. Failures keep the gap pending and retry with backoff; a newer
  // resync or a reset replaces it, and reconnect resumes it via onopen.
  function runGapFill() {
    if (!pendingGap || pendingGap.running) return;
    var myGen = streamGen;
    var gap = pendingGap;
    gap.running = true;
    fetch("/api/briefings?after=" + encodeURIComponent(gap.cursor) + "&limit=50")
      .then(function (r) { if (!r.ok) throw new Error("briefings " + r.status); return r.json(); })
      .then(function (rows) {
        if (!pendingGap || myGen !== streamGen) return;
        rows = rows || [];
        rows.forEach(upsertBriefing);
        renderBriefs();
        if (rows.length) gap.cursor = String(rows[rows.length - 1].seq);
        gap.running = false;
        if (rows.length < 50 || Number(gap.cursor) >= gap.boundary) {
          lastEventId = String(Math.max(Number(gap.cursor), gap.liveMax || 0));
          pendingGap = null;
        } else {
          gap.timer = setTimeout(runGapFill, 100);
        }
      })
      .catch(function () {
        if (!pendingGap || myGen !== streamGen) return;
        gap.running = false;
        gap.timer = setTimeout(runGapFill, 3000);
      });
  }

  // doReset handles an invalid cursor (a restored database ends below it,
  // or more was missed than the client holds): replace state with the
  // latest page, reset the cursor to the resync maximum, and reopen the
  // stream so the browser's own reconnect cursor cannot stay stale.
  function doReset(maxSeq) {
    if (pendingGap && pendingGap.timer) clearTimeout(pendingGap.timer);
    pendingGap = null;
    fetch("/api/briefings?limit=50")
      .then(function (r) { if (!r.ok) throw new Error("briefings " + r.status); return r.json(); })
      .then(function (rows) {
        state.briefings = [];
        (rows || []).forEach(upsertBriefing);
        renderBriefs();
        lastEventId = String(maxSeq);
        connect();
      })
      .catch(function () { /* the next resync or reconnect retries the reset */ });
  }

  function onResync(ev) {
    var max = NaN;
    try { max = Number(JSON.parse(ev.data).max_seq); } catch (e) { /* keep NaN */ }
    if (isNaN(max)) max = 0;
    var cur = Number(lastEventId) || 0;
    if (!lastEventId || max <= cur || max - cur > 500) {
      doReset(max);
      return;
    }
    pendingGap = {cursor: lastEventId, boundary: max, liveMax: 0, running: false, timer: 0};
    runGapFill();
  }

  function connect() {
    if (src) { try { src.close(); } catch (e) {} }
    streamGen++;
    src = new EventSource("/events");
    src.onopen = function () {
      connEl.textContent = "live"; connEl.className = "conn live";
      if (pendingGap) runGapFill();
    };
    src.onerror = function () { connEl.textContent = "reconnecting"; connEl.className = "conn"; };
    src.addEventListener("briefing", function (ev) {
      try {
        upsertBriefing(JSON.parse(ev.data));
        var seq = ev.lastEventId ? Number(ev.lastEventId) : NaN;
        if (pendingGap) {
          if (!isNaN(seq)) pendingGap.liveMax = Math.max(pendingGap.liveMax || 0, seq);
        } else if (ev.lastEventId) {
          lastEventId = ev.lastEventId;
        }
      } catch (e) { /* keep old state */ }
    });
    src.addEventListener("overview", function () { /* Step 20 renders this; the stub carries no state */ });
    src.addEventListener("resync", onResync);
    src.addEventListener("group", function (ev) {
      try {
        state.groups = JSON.parse(ev.data) || [];
        renderGroups();
      } catch (e) { /* keep old state */ }
    });
  }

  function boot() {
    ["filter-priority", "filter-host", "filter-text"].forEach(function (id) {
      document.getElementById(id).addEventListener("input", renderBriefs);
    });
    setInterval(tickCountdowns, 1000);
    fetch("/api/briefings?limit=50").then(function (r) { return r.json(); }).then(function (rows) {
      state.briefings = rows || [];
      renderBriefs();
    }).catch(function () { /* SSE will fill in */ });
    connect();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
