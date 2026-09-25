// Theme: the system preference unless the operator chose one here; Shift-D
// (outside a field) or the header button flips it, and the choice is kept
// in this browser. The <html> data-theme attribute is what the stylesheet
// reads; it is set before first paint by the inline-free loader below.
(function () {
  var root = document.documentElement;
  var KEY = "dialler-admin-theme";
  function stored() { try { return localStorage.getItem(KEY); } catch (e) { return null; } }
  function system() { return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"; }
  function apply(theme) { root.setAttribute("data-theme", theme); }
  function current() { return root.getAttribute("data-theme") || system(); }
  function set(theme) { apply(theme); try { localStorage.setItem(KEY, theme); } catch (e) {} }
  apply(stored() || system());
  if (window.matchMedia) {
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", function () { if (!stored()) apply(system()); });
  }
  document.addEventListener("keydown", function (e) {
    if (!e.shiftKey || (e.key !== "D" && e.key !== "d") || e.metaKey || e.ctrlKey || e.altKey) return;
    var t = e.target && e.target.tagName;
    if (t === "INPUT" || t === "TEXTAREA" || t === "SELECT" || (e.target && e.target.isContentEditable)) return;
    e.preventDefault();
    set(current() === "dark" ? "light" : "dark");
  });
  document.addEventListener("click", function (e) {
    var b = e.target && e.target.closest && e.target.closest("[data-toggle-theme]");
    if (b) { e.preventDefault(); set(current() === "dark" ? "light" : "dark"); }
  });
})();
