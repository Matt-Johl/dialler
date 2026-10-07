// The page follows the system's light or dark. The button overrides it; the
// override lapses when it matches the system again or the system changes.
(function () {
  var root = document.documentElement;
  var system = matchMedia("(prefers-color-scheme: dark)");
  function follow() {
    delete root.dataset.theme;
    try { localStorage.removeItem("theme"); } catch (e) {}
  }
  document.querySelector(".theme").addEventListener("click", function () {
    var dark = root.dataset.theme ? root.dataset.theme === "dark" : system.matches;
    var next = dark ? "light" : "dark";
    if ((next === "dark") === system.matches) return follow();
    root.dataset.theme = next;
    try { localStorage.setItem("theme", next); } catch (e) {}
  });
  system.addEventListener("change", follow);
})();

// The hero phone shows the app's screens in turn, every 4 s. A dot shows its
// screen and the turn carries on from there after a full 4 s. A preference
// for reduced motion stops the turn; the dots still work.
(function () {
  var shots = document.querySelectorAll(".iphone .shot");
  var dots = document.querySelectorAll(".dots button");
  if (!shots.length || shots.length !== dots.length) return;
  var current = 0, timer = null;
  function show(i) {
    shots[current].classList.remove("active");
    dots[current].setAttribute("aria-pressed", "false");
    current = i;
    shots[current].classList.add("active");
    dots[current].setAttribute("aria-pressed", "true");
  }
  function start() { if (!timer) timer = setInterval(function () { show((current + 1) % shots.length); }, 4000); }
  function stop() { clearInterval(timer); timer = null; }
  var stopped = matchMedia("(prefers-reduced-motion: reduce)").matches;
  dots.forEach(function (dot, i) {
    dot.addEventListener("click", function () { stop(); show(i); if (!stopped) start(); });
  });
  document.addEventListener("visibilitychange", function () { if (document.hidden) stop(); else if (!stopped) start(); });
  if (!stopped) start();
})();
