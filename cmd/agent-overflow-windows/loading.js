// Served as /loading.js to /loading and the picker. Polls /loading.json
// every 500 ms and writes the report into the page's ao-loading-* elements.
// A page keeps its own status text until the backend reports a phase. The
// poll pauses while the document is hidden and asks at once when it is
// shown again. Behavior is tested in
// frontend/src/lib/transport/launcherLoadingScript.test.ts.
(function () {
  "use strict";
  var title = document.getElementById("ao-loading-title");
  var status = document.getElementById("ao-loading-status");
  var meta = document.getElementById("ao-loading-meta");
  var timer = null;
  var inFlight = false;
  function clock(ms) {
    var s = Math.floor(ms / 1000);
    return Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0");
  }
  function render(r) {
    if (title && r.title) title.textContent = r.title;
    if (status && r.phase) status.textContent = r.status;
    if (!meta) return;
    var parts = [];
    if (r.steps > 0) parts.push("Step " + r.step + " of " + r.steps);
    if (r.elapsedMs >= 1000) parts.push(clock(r.elapsedMs) + " elapsed");
    meta.textContent = parts.join(" · ");
  }
  function hidden() {
    return document.visibilityState === "hidden";
  }
  function cancel() {
    if (timer === null) return;
    clearTimeout(timer);
    timer = null;
  }
  function poll() {
    timer = null;
    inFlight = true;
    fetch("/loading.json", { cache: "no-store" })
      .then(function (res) { return res.ok ? res.json() : null; })
      .then(function (r) { if (r) render(r); })
      .catch(function () {})
      .then(function () {
        inFlight = false;
        if (!hidden()) timer = setTimeout(poll, 500);
      });
  }
  document.addEventListener("visibilitychange", function () {
    cancel();
    if (!hidden() && !inFlight) poll();
  });
  if (!hidden()) poll();
})();
