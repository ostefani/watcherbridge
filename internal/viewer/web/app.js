// WatcherBridge viewer: live briefing feed over SSE. Every string from
// the server is rendered with textContent — never raw-HTML insertion —
// so hostile hostnames and prompt-injection banners stay inert text.
(function () {
  "use strict";
  // groupSkew corrects the client clock to the server clock: the group
  // wire carries per-row now_unix, and the first row's value minus the
  // client time at snapshot arrival is the offset the countdown uses.
  // detached marks a history-shifted window: an explicit older-page load
  // slid the 500-row window down, so Back to live is offered until a
  // reset re-anchors on the latest page.
  var state = { briefings: [], groups: [], selected: null, groupSkew: 0, detached: false };
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

  // stripPrefix drops the "kind:" anchor prefix operators never say out
  // loud ("host:nebula-1.local" renders as "nebula-1.local"). Mirrors the
  // Go stripAnchorPrefix helper; both must agree.
  function stripPrefix(s) {
    s = String(s === undefined || s === null ? "" : s);
    var i = s.indexOf(":");
    return i > 0 ? s.slice(i + 1) : s;
  }

  function plural(n, unit) {
    return n + " " + unit + (n === 1 ? "" : "s");
  }

  // renderGroups is status-driven (Designv8 §2.3): open groups show the
  // countdown bar, processing groups render as "ANALYSING…" with no bar,
  // so a group never restarts its fraction when a snapshot arrives —
  // the 1s client tick animates the bar between snapshots.
  function renderGroups() {
    var ul = document.getElementById("group-list");
    while (ul.firstChild) ul.removeChild(ul.firstChild);
    state.groups.forEach(function (g) {
      var li = el("li", null, null);
      var label = stripPrefix(g.anchor);
      if (g.status === "processing") {
        li.appendChild(el("span", "pill medium", "ANALYSING"));
        li.appendChild(el("span", "meta", label + " · " + plural(g.event_count, "event") + " · analysing…"));
        li.setAttribute("data-static", "1");
      } else if (g.status === "closed") {
        li.appendChild(el("span", "pill medium", "QUEUED"));
        li.appendChild(el("span", "meta", label + " · " + plural(g.event_count, "event") + " · queued…"));
        li.setAttribute("data-static", "1");
      } else {
        li.appendChild(el("span", "pill medium", "OPEN"));
        li.appendChild(el("span", "meta", null));
        var bar = el("div", "countbar", null);
        bar.appendChild(el("div", null, null));
        li.appendChild(bar);
        li.setAttribute("data-deadline", String(g.deadline_unix));
        li.setAttribute("data-lastevent", String(g.last_event_unix));
        li.setAttribute("data-label", label);
        li.setAttribute("data-count", String(g.event_count));
      }
      ul.appendChild(li);
    });
    tickCountdowns();
  }

  // tickCountdowns animates the open-group bars between snapshots. now is
  // corrected by the server clock skew measured at snapshot arrival (the
  // client never stamps its own "opened" time), and the fraction follows
  // Designv8 §2.3: clamp((deadline − now) / max(1, deadline − last_event)).
  function tickCountdowns() {
    var now = Math.floor(Date.now() / 1000) + (state.groupSkew || 0);
    var items = document.getElementById("group-list").children;
    for (var i = 0; i < items.length; i++) {
      var li = items[i];
      if (li.getAttribute("data-static")) continue;
      var deadline = parseInt(li.getAttribute("data-deadline"), 10);
      var lastEvent = parseInt(li.getAttribute("data-lastevent"), 10);
      if (isNaN(lastEvent)) lastEvent = deadline;
      var left = Math.max(0, deadline - now);
      var total = Math.max(1, deadline - lastEvent);
      var fill = li.querySelector(".countbar > div");
      if (fill) fill.style.width = (100 * left / total).toFixed(1) + "%";
      var meta = li.querySelector(".meta");
      if (meta) {
        meta.textContent = li.getAttribute("data-label") + " · " +
          plural(parseInt(li.getAttribute("data-count"), 10) || 0, "event") +
          " · closes in " + left + "s";
      }
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

  // MAX_BRIEFINGS caps the in-memory list (Designv8 §2.5): the list is
  // newest-first by seq. Live and recovery merges anchor at the newest
  // end and evict the oldest; explicit history loads anchor at the oldest
  // end and evict the newest (page replacement), so fetching older pages
  // while full moves the window down instead of discarding the fetch.
  // Anything evicted stays pageable through ?before=.
  var MAX_BRIEFINGS = 500;
  function bySeqDesc(a, b) { return (b.seq || 0) - (a.seq || 0); }
  function normalizeBriefings() {
    state.briefings.sort(bySeqDesc);
    if (state.briefings.length > MAX_BRIEFINGS) state.briefings.length = MAX_BRIEFINGS;
    reconcileSelection();
  }

  function upsertBriefing(b) {
    var found = false;
    state.briefings = state.briefings.map(function (x) {
      if (x.briefing_id === b.briefing_id) { found = true; return b; }
      return x;
    });
    if (!found) state.briefings.unshift(b);
    normalizeBriefings();
    renderBriefs();
    if (state.selected === b.briefing_id) selectBriefing(b.briefing_id);
  }

  var src = null;
  var lastEventId = "";
  // pending is the single recovery slot: {id, kind, ...}. Every resync
  // replaces it (clearing the old timer) and bumps the identity, and every
  // async continuation checks id before mutating, so an older request can
  // never complete over or clear a newer recovery. kind is "gap" (page
  // toward the resync boundary) or "reset" (latest page replaces state).
  var pending = null;
  var opSeq = 0;

  function clearPending(op) {
    if (pending && pending.id === op.id) {
      if (pending.timer) clearTimeout(pending.timer);
      pending = null;
    }
  }

  // upsertMany batches one page into a single state update: the per-row
  // upsert copies the whole list, which multiplies DOM work during a
  // multi-page recovery. Gap rows are new by construction; overlap with
  // live upserts shares the briefing id and is skipped. Recovery pages
  // arrive ascending, so the merge normalizes back to newest-first before
  // trimming — otherwise the trim keeps the oldest rows and discards the
  // newest recovery while the cursor still advances past them.
  function upsertMany(rows) {
    var seen = {};
    state.briefings.forEach(function (b) { seen[b.briefing_id] = true; });
    var fresh = [];
    rows.forEach(function (b) {
      if (!b || seen[b.briefing_id]) return;
      seen[b.briefing_id] = true;
      fresh.push(b);
    });
    if (fresh.length) {
      state.briefings = state.briefings.concat(fresh);
      normalizeBriefings();
      renderBriefs();
    }
  }

  // upsertHistory merges one explicitly requested older page. Newest-first
  // order holds, but on overflow the eviction comes off the newest end:
  // the operator asked to look back, so the window slides down instead of
  // throwing the fetched rows away and refetching the same page forever.
  // Recovery merges (upsertMany) keep the opposite anchor — live data wins
  // over history while a gap is being repaired.
  function upsertHistory(rows) {
    var seen = {};
    state.briefings.forEach(function (b) { seen[b.briefing_id] = true; });
    var fresh = [];
    rows.forEach(function (b) {
      if (!b || seen[b.briefing_id]) return;
      seen[b.briefing_id] = true;
      fresh.push(b);
    });
    if (!fresh.length) return 0;
    state.briefings = state.briefings.concat(fresh);
    state.briefings.sort(bySeqDesc);
    var overflow = state.briefings.length - MAX_BRIEFINGS;
    if (overflow > 0) state.briefings.splice(0, overflow);
    reconcileSelection();
    renderBriefs();
    return fresh.length;
  }

  function updateLiveBtn() {
    var btn = document.getElementById("live");
    if (btn) btn.style.display = state.detached ? "" : "none";
  }

  // loadOlder pages history the 500-row cap evicted: ?before=<oldest seq>
  // returns the next older page ascending, and the merge slides the
  // window down. A short page means history is exhausted. Guarded against
  // concurrent recovery: a pending gap-fill owns the list until it lands.
  function loadOlder() {
    if (pending) return;
    var btn = document.getElementById("older");
    var min = Infinity;
    state.briefings.forEach(function (b) {
      if (b && typeof b.seq === "number" && b.seq < min) min = b.seq;
    });
    if (min === Infinity) return;
    if (btn) btn.disabled = true;
    fetch("/api/briefings?before=" + encodeURIComponent(String(min)) + "&limit=50")
      .then(function (r) { if (!r.ok) throw new Error("briefings " + r.status); return r.json(); })
      .then(function (rows) {
        rows = rows || [];
        var added = upsertHistory(rows);
        if (added > 0) {
          state.detached = true;
          updateLiveBtn();
        }
        if (btn) {
          if (rows.length < 50) {
            btn.textContent = "No older briefings";
          } else {
            btn.disabled = false;
          }
        }
      })
      .catch(function () { if (btn) btn.disabled = false; });
  }

  // backToLive re-anchors the window on the latest page through the same
  // reset machinery as resync recovery: the snapshot replaces state and
  // the tail covers live rows that arrive mid-fetch. Guarded like
  // loadOlder — a pending recovery already owns the list.
  function backToLive() {
    if (pending) return;
    pending = {id: ++opSeq, kind: "reset", boundary: 0, liveMax: 0, running: false, timer: 0};
    runReset(pending);
  }

  // runGapFill pages ?after= toward the resync boundary, a calm page at a
  // time. Failures keep the gap pending and retry with backoff; a newer
  // resync replaces it, and reconnect resumes it via onopen. Live events
  // upsert data but never move lastEventId past the unrecovered range.
  function runGapFill(op) {
    if (!pending || pending.id !== op.id || op.running) return;
    op.running = true;
    fetch("/api/briefings?after=" + encodeURIComponent(op.cursor) + "&limit=50")
      .then(function (r) { if (!r.ok) throw new Error("briefings " + r.status); return r.json(); })
      .then(function (rows) {
        if (!pending || pending.id !== op.id) return;
        rows = rows || [];
        upsertMany(rows);
        if (rows.length) op.cursor = String(rows[rows.length - 1].seq);
        op.running = false;
        if (rows.length < 50 || Number(op.cursor) >= op.boundary) {
          lastEventId = String(Math.max(Number(op.cursor), op.liveMax || 0));
          clearPending(op);
        } else {
          op.timer = setTimeout(function () { runGapFill(op); }, 100);
        }
      })
      .catch(function () {
        if (!pending || pending.id !== op.id) return;
        op.running = false;
        op.timer = setTimeout(function () { runGapFill(op); }, 3000);
      });
  }

  // runReset replaces state with the latest page and recovers the tail
  // after the snapshot cursor. The stream stays open throughout: live
  // events accumulate into liveMax, and the tail is paged in with the gap
  // machinery instead of reopening the stream (a fresh EventSource without
  // Last-Event-ID would skip those events forever). Failures stay pending
  // with backoff retry like gap recovery.
  function runReset(op) {
    if (!pending || pending.id !== op.id || op.running) return;
    op.running = true;
    fetch("/api/briefings?limit=50")
      .then(function (r) { if (!r.ok) throw new Error("briefings " + r.status); return r.json(); })
      .then(function (rows) {
        if (!pending || pending.id !== op.id) return;
        rows = rows || [];
        state.briefings = [];
        upsertMany(rows);
        state.detached = false; // a reset re-anchors on the latest page
        updateLiveBtn();
        renderBriefs(); // replacement renders even when empty: no stale rows survive
        reconcileSelection();
        var pageMax = 0;
        rows.forEach(function (b) { if (b && b.seq > pageMax) pageMax = b.seq; });
        op.running = false;
        if (op.liveMax > pageMax) {
          pending = {id: op.id, kind: "gap", cursor: String(pageMax),
            boundary: op.liveMax, liveMax: op.liveMax, running: false, timer: 0};
          runGapFill(pending);
        } else {
          lastEventId = String(Math.max(pageMax, op.liveMax || 0));
          clearPending(op);
        }
      })
      .catch(function () {
        if (!pending || pending.id !== op.id) return;
        op.running = false;
        op.timer = setTimeout(function () { runReset(op); }, 3000);
      });
  }

  function onResync(ev) {
    var max = NaN;
    try { max = Number(JSON.parse(ev.data).max_seq); } catch (e) { /* keep NaN */ }
    if (isNaN(max)) max = 0;
    if (pending && pending.timer) clearTimeout(pending.timer);
    var cur = Number(lastEventId) || 0;
    if (!lastEventId || max <= cur || max - cur > 500) {
      pending = {id: ++opSeq, kind: "reset", boundary: max, liveMax: 0, running: false, timer: 0};
      runReset(pending);
      return;
    }
    pending = {id: ++opSeq, kind: "gap", cursor: lastEventId, boundary: max,
      liveMax: 0, running: false, timer: 0};
    runGapFill(pending);
  }

  function connect() {
    if (src) { try { src.close(); } catch (e) {} }
    src = new EventSource("/events");
    src.onopen = function () {
      connEl.textContent = "live"; connEl.className = "conn live";
      // The server subscribes before flushing its first byte, so onopen
      // is the earliest moment a snapshot read is ordered after the
      // subscription: anything committed before it is in the snapshot,
      // anything after arrives live, and the tail covers the overlap.
      // Starting the boot snapshot here (rather than right after
      // connect()) closes the subscribe-before-snapshot race.
      if (!pending || pending.running) return;
      if (pending.awaitOpen) pending.awaitOpen = false;
      if (pending.kind === "gap") runGapFill(pending); else runReset(pending);
    };
    src.onerror = function () { connEl.textContent = "reconnecting"; connEl.className = "conn"; };
    src.addEventListener("briefing", function (ev) {
      try {
        upsertBriefing(JSON.parse(ev.data));
        var seq = ev.lastEventId ? Number(ev.lastEventId) : NaN;
        if (pending) {
          if (!isNaN(seq)) pending.liveMax = Math.max(pending.liveMax || 0, seq);
        } else if (ev.lastEventId) {
          lastEventId = ev.lastEventId;
        }
      } catch (e) { /* keep old state */ }
    });
    src.addEventListener("overview", function () { /* Step 20 renders this; the stub carries no state */ });
    src.addEventListener("resync", onResync);
    src.addEventListener("group", function (ev) {
      try {
        var rows = JSON.parse(ev.data) || [];
        if (rows.length && typeof rows[0].now_unix === "number") {
          state.groupSkew = Math.floor(rows[0].now_unix - Date.now() / 1000);
        }
        state.groups = rows;
        renderGroups();
      } catch (e) { /* keep old state */ }
    });
  }

  // reconcileSelection clears the detail pane when its briefing is gone
  // from the replaced state; selectBriefing shows the hint for unknown ids.
  function reconcileSelection() {
    var found = false;
    state.briefings.forEach(function (b) { if (b.briefing_id === state.selected) found = true; });
    if (!found) selectBriefing(null);
  }

  function boot() {
    ["filter-priority", "filter-host", "filter-text"].forEach(function (id) {
      document.getElementById(id).addEventListener("input", renderBriefs);
    });
    document.getElementById("older").addEventListener("click", loadOlder);
    document.getElementById("live").addEventListener("click", backToLive);
    updateLiveBtn();
    setInterval(tickCountdowns, 1000);
    // Initialization runs through the same snapshot/tail recovery as a
    // reset, with operation ownership and retry: a standalone fetch could
    // resolve after live events arrived and wipe them, with the cursor
    // already past the snapshot. The snapshot does not start here:
    // connect() only opens the stream, and onopen starts the fetch once
    // the server-side subscription is established. Live events can only
    // arrive after that point, so they accumulate into liveMax during
    // the fetch and the tail covers them.
    pending = {id: ++opSeq, kind: "reset", boundary: 0, liveMax: 0,
      running: false, timer: 0, awaitOpen: true};
    connect();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
