document.querySelector(".theme").addEventListener("click", function () {
  var root = document.documentElement;
  var dark = root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
  var next = dark ? "light" : "dark";
  root.dataset.theme = next;
  try { localStorage.setItem("theme", next); } catch (e) {}
});

// The hero phone shows the app's screens in turn, every 4 s. A dot shows its
// screen and stops the turn; so does a preference for reduced motion.
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
    dot.addEventListener("click", function () { stopped = true; stop(); show(i); });
  });
  document.addEventListener("visibilitychange", function () { if (document.hidden) stop(); else if (!stopped) start(); });
  if (!stopped) start();
})();
