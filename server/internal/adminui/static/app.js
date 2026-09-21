// dialler-admin: what the pages do on top of plain forms. Everything here
// enhances a page that already works without it — the forms still post,
// the links still lead to their own pages — so a failure here degrades to
// the plain version rather than to nothing.
(() => {
  "use strict";

  // Motion is opt-out for people who ask for less of it.
  const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  document.documentElement.classList.add("js");
  requestAnimationFrame(() => document.body.classList.add("ready"));

  // ---- Flash messages slide in, then leave on their own.
  document.querySelectorAll(".flash").forEach((el) => {
    el.classList.add("in");
    if (el.classList.contains("notice")) {
      setTimeout(() => el.classList.add("out"), 4200);
      el.addEventListener("transitionend", () => { if (el.classList.contains("out")) el.remove(); });
    }
  });

  // ---- A form that knows when it has unsaved changes: the save bar wakes
  // up, and leaving the page asks first.
  document.querySelectorAll("[data-dirty-form]").forEach((form) => {
    const bar = form.querySelector("[data-savebar]");
    const note = form.querySelector("[data-savebar-note]");
    const button = form.querySelector("[data-savebar-button]");
    const initial = new FormData(form);
    const snapshot = (fd) => [...fd.entries()].filter(([k]) => k !== "_csrf").map(([k, v]) => `${k}=${v}`).join("&");
    const base = snapshot(initial);
    let dirty = false;
    const update = () => {
      dirty = snapshot(new FormData(form)) !== base;
      bar.classList.toggle("active", dirty);
      button.disabled = !dirty;
      note.textContent = dirty ? "Unsaved changes" : "No unsaved changes";
    };
    form.addEventListener("input", update);
    form.addEventListener("change", update);
    form.addEventListener("submit", () => { dirty = false; button.disabled = true; note.textContent = "Saving…"; });
    window.addEventListener("beforeunload", (e) => { if (dirty) { e.preventDefault(); e.returnValue = ""; } });
  });

  // ---- Destructive links open a dialog instead of a page.
  const modal = document.querySelector("[data-modal]");
  if (modal && typeof modal.showModal === "function") {
    const form = modal.querySelector("form");
    document.querySelectorAll("[data-confirm]").forEach((link) => {
      link.addEventListener("click", (e) => {
        if (link.getAttribute("aria-disabled") === "true") return;
        e.preventDefault();
        modal.querySelector("[data-modal-title]").textContent = link.dataset.confirm;
        modal.querySelector("[data-modal-message]").textContent = link.dataset.confirmMessage || "";
        modal.querySelector("[data-modal-button]").textContent = link.dataset.confirmButton || "Confirm";
        form.action = link.getAttribute("href");
        modal.showModal();
      });
    });
    modal.querySelector("[data-modal-cancel]").addEventListener("click", () => modal.close());
    modal.addEventListener("click", (e) => { if (e.target === modal) modal.close(); });
  }

  // ---- The directory, edited in place.
  const dir = document.querySelector("[data-directory]");
  if (!dir) return;
  const device = dir.dataset.device;
  const csrf = dir.dataset.csrf;
  const rows = dir.querySelector("[data-rows]");
  const empty = dir.querySelector("[data-empty]");
  const count = dir.querySelector("[data-count]");

  const escape = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const viaLabel = (mode) => (mode === "trunk" ? "PBX" : "This server");
  const refresh = () => {
    const n = rows.querySelectorAll("tr[data-contact]").length;
    count.textContent = n;
    empty.hidden = n > 0;
  };

  // A row's two faces: the read view, and the editor with a Save and a
  // Cancel (and Delete for an existing contact).
  const readView = (tr, c) => {
    tr.dataset.id = c.id; tr.dataset.name = c.display_name; tr.dataset.uri = c.uri; tr.dataset.mode = c.mode; tr.dataset.favourite = c.favourite ? "1" : "";
    tr.innerHTML =
      `<td data-cell="name">${escape(c.display_name)}</td>` +
      `<td data-cell="uri" class="mono">${escape(c.uri)}</td>` +
      `<td data-cell="mode" class="dim">${viaLabel(c.mode)}</td>` +
      `<td data-cell="fav" class="centre">${c.favourite ? '<span class="star">★</span>' : ""}</td>` +
      `<td class="right"><a class="chevron" href="/devices/${encodeURIComponent(device)}/contacts/${encodeURIComponent(c.id)}/edit" data-edit>Edit</a></td>`;
    tr.classList.remove("editing");
    wire(tr);
  };

  const editView = (tr, c, isNew) => {
    tr.classList.add("editing");
    tr.innerHTML =
      `<td><input name="display_name" value="${escape(c.display_name)}" placeholder="Name" required></td>` +
      `<td><input name="uri" value="${escape(c.uri)}" class="mono" placeholder="100, or sip:100@pbx" required></td>` +
      `<td><select name="mode"><option value="local"${c.mode !== "trunk" ? " selected" : ""}>This server</option><option value="trunk"${c.mode === "trunk" ? " selected" : ""}>PBX</option></select></td>` +
      `<td class="centre"><input type="checkbox" name="favourite" aria-label="Favourite"${c.favourite ? " checked" : ""}></td>` +
      `<td class="right nowrap">` +
        `<button type="button" class="small primary" data-save>${isNew ? "Add" : "Save"}</button> ` +
        `<button type="button" class="small ghost" data-cancel>Cancel</button>` +
        (isNew ? "" : ` <button type="button" class="small danger ghost" data-delete>Delete</button>`) +
      `</td>`;
    const first = tr.querySelector("input[name=display_name]");
    first.focus();
    tr.querySelector("[data-cancel]").addEventListener("click", () => {
      if (isNew) { tr.classList.add("leaving"); setTimeout(() => { tr.remove(); refresh(); }, reduced ? 0 : 180); }
      else readView(tr, c);
    });
    tr.querySelector("[data-save]").addEventListener("click", () => save(tr, c, isNew));
    tr.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && e.target.tagName !== "SELECT") { e.preventDefault(); save(tr, c, isNew); }
      if (e.key === "Escape") tr.querySelector("[data-cancel]").click();
    });
    const del = tr.querySelector("[data-delete]");
    if (del) del.addEventListener("click", () => remove(tr, c));
  };

  const request = async (path, body) => {
    const res = await fetch(path, { method: "POST", credentials: "same-origin", headers: { "X-Requested-With": "fetch" }, body });
    let data = {};
    try { data = await res.json(); } catch (_) { /* a redirect or an error page */ }
    if (!res.ok) throw new Error(data.error || `The server answered ${res.status}.`);
    return data;
  };

  const problem = (tr, message) => {
    tr.classList.add("shake");
    setTimeout(() => tr.classList.remove("shake"), 500);
    let note = tr.querySelector(".row-error");
    if (!note) { note = document.createElement("div"); note.className = "row-error"; tr.querySelector("td").appendChild(note); }
    note.textContent = message;
  };

  const save = async (tr, c, isNew) => {
    const body = new FormData();
    body.set("_csrf", csrf);
    body.set("display_name", tr.querySelector("[name=display_name]").value);
    body.set("uri", tr.querySelector("[name=uri]").value);
    body.set("mode", tr.querySelector("[name=mode]").value);
    if (tr.querySelector("[name=favourite]").checked) body.set("favourite", "on");
    tr.classList.add("busy");
    try {
      const path = `/devices/${encodeURIComponent(device)}/contacts${isNew ? "" : "/" + encodeURIComponent(c.id)}`;
      const { contact } = await request(path, body);
      readView(tr, contact);
      tr.classList.add("saved");
      setTimeout(() => tr.classList.remove("saved"), 1200);
      refresh();
    } catch (err) {
      problem(tr, err.message);
    } finally {
      tr.classList.remove("busy");
    }
  };

  const remove = async (tr, c) => {
    const body = new FormData();
    body.set("_csrf", csrf);
    tr.classList.add("busy");
    try {
      await request(`/devices/${encodeURIComponent(device)}/contacts/${encodeURIComponent(c.id)}/delete`, body);
      tr.classList.add("leaving");
      setTimeout(() => { tr.remove(); refresh(); }, reduced ? 0 : 180);
    } catch (err) {
      tr.classList.remove("busy");
      problem(tr, err.message);
    }
  };

  const fromRow = (tr) => ({ id: tr.dataset.id, display_name: tr.dataset.name, uri: tr.dataset.uri, mode: tr.dataset.mode, favourite: tr.dataset.favourite === "1" });

  const wire = (tr) => {
    const open = (e) => {
      if (tr.classList.contains("editing")) return;
      if (e && e.target.closest("a, button, input, select")) e.preventDefault();
      editView(tr, fromRow(tr), false);
    };
    tr.addEventListener("click", open);
    tr.querySelector("[data-edit]").addEventListener("click", (e) => { e.preventDefault(); open(); });
  };
  rows.querySelectorAll("tr[data-contact]").forEach(wire);

  const add = dir.querySelector("[data-add-contact]");
  add.addEventListener("click", (e) => {
    e.preventDefault();
    if (rows.querySelector("tr.editing[data-new]")) return;
    const tr = document.createElement("tr");
    tr.setAttribute("data-contact", "");
    tr.setAttribute("data-new", "");
    tr.classList.add("entering");
    rows.appendChild(tr);
    editView(tr, { id: "", display_name: "", uri: "", mode: "local", favourite: false }, true);
    requestAnimationFrame(() => tr.classList.remove("entering"));
    empty.hidden = true;
  });
})();
