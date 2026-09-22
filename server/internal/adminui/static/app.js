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
      button.hidden = !dirty;
      note.textContent = dirty ? "Unsaved changes" : "";
    };
    update();
    form.addEventListener("input", update);
    form.addEventListener("change", update);
    form.addEventListener("submit", (e) => {
      // Only the Save button's submit is the settings form; other submit
      // buttons in the header carry their own formaction.
      if (e.submitter && e.submitter !== button && e.submitter.hasAttribute("formaction")) return;
      dirty = false; button.disabled = true; note.textContent = "Saving…";
    });
    window.addEventListener("beforeunload", (e) => { if (dirty) { e.preventDefault(); e.returnValue = ""; } });
  });

  // ---- A filter box narrows a list as you type.
  document.querySelectorAll("[data-filter]").forEach((input) => {
    const rows = document.querySelectorAll(input.dataset.filter);
    input.addEventListener("input", () => {
      const q = input.value.trim().toLowerCase();
      rows.forEach((r) => { r.hidden = q !== "" && !(r.dataset.text || r.textContent).toLowerCase().includes(q); });
    });
  });

  // ---- The CSV picker submits as soon as a file is chosen.
  document.querySelectorAll("[data-upload]").forEach((form) => {
    const file = form.querySelector("[data-upload-file]");
    form.querySelector("[data-upload-submit]").hidden = true;
    file.addEventListener("change", () => { if (file.files.length) form.submit(); });
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

  const pencil = (rows.querySelector("[data-edit]") || {}).innerHTML || "Edit";
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
      `<td data-cell="mode" class="muted">${viaLabel(c.mode)}</td>` +
      `<td data-cell="fav" class="c">${c.favourite ? '<span class="star">★</span>' : ""}</td>` +
      `<td class="r"><a class="edit-link" href="/devices/${encodeURIComponent(device)}/contacts/${encodeURIComponent(c.id)}/edit" data-edit aria-label="Edit">${pencil}</a></td>`;
    tr.classList.remove("editing");
    wire(tr);
  };

  const editView = (tr, c, isNew) => {
    tr.classList.add("editing");
    tr.innerHTML =
      `<td><input name="display_name" value="${escape(c.display_name)}" placeholder="Name" required></td>` +
      `<td><input name="uri" value="${escape(c.uri)}" class="mono" placeholder="100, or sip:100@pbx" required></td>` +
      `<td><select name="mode"><option value="local"${c.mode !== "trunk" ? " selected" : ""}>This server</option><option value="trunk"${c.mode === "trunk" ? " selected" : ""}>PBX</option></select></td>` +
      `<td class="c"><input type="checkbox" name="favourite" aria-label="Favourite"${c.favourite ? " checked" : ""}></td>` +
      `<td class="r nowrap">` +
        `<button type="button" class="btn primary sm" data-save>${isNew ? "Add" : "Save"}</button> ` +
        `<button type="button" class="btn ghost sm" data-cancel>Cancel</button>` +
        (isNew ? "" : ` <button type="button" class="btn danger ghost sm" data-delete>Delete</button>`) +
      `</td>`;
    const first = tr.querySelector("input[name=display_name]");
    first.focus();
    tr.querySelector("[data-cancel]").addEventListener("click", (e) => {
      e.stopPropagation();
      // Cancel writes nothing: the row goes back to the contact as it
      // was when the editor opened.
      if (isNew) { tr.classList.add("leaving"); setTimeout(() => { tr.remove(); refresh(); }, reduced ? 0 : 180); }
      else readView(tr, c);
    });
    tr.querySelector("[data-save]").addEventListener("click", (e) => { e.stopPropagation(); save(tr, c, isNew); });
    const del = tr.querySelector("[data-delete]");
    if (del) del.addEventListener("click", (e) => { e.stopPropagation(); remove(tr, c); });
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
    if (tr.dataset.wired) return;
    tr.dataset.wired = "1";
    tr.addEventListener("click", (e) => {
      if (tr.classList.contains("editing")) return;
      if (e.target.closest("[data-edit]")) e.preventDefault();
      editView(tr, fromRow(tr), false);
    });
    // Enter and Escape act through the editor's buttons, so they carry
    // whichever contact that editor was opened with.
    tr.addEventListener("keydown", (e) => {
      if (!tr.classList.contains("editing")) return;
      if (e.key === "Enter" && e.target.tagName !== "SELECT") { e.preventDefault(); tr.querySelector("[data-save]").click(); }
      if (e.key === "Escape") tr.querySelector("[data-cancel]").click();
    });
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
    wire(tr);
    editView(tr, { id: "", display_name: "", uri: "", mode: "local", favourite: false }, true);
    requestAnimationFrame(() => tr.classList.remove("entering"));
    empty.hidden = true;
  });
})();
