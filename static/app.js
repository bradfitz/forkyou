// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Fragments: a few dozen lines doing the part of htmx this program uses.
//
// Pages arrive whole from the server, with everything on them already
// drawn. Requests are made only when the reviewer acts (opens a comment
// form, saves a draft, marks a file reviewed), and each answers with the
// HTML of whatever it changed, which replaces the old copy in place.
//
//	data-get="url" / data-post="url"   what to fetch (a form posts its fields)
//	data-target="this" | "closest sel" | "#id"   what the answer replaces
//	data-swap="outerHTML" | "innerHTML" | "afterend" | "beforeend"
//	data-remove="closest sel"   remove that element, no request at all

"use strict";

(function () {
	function target(el, spec) {
		if (!spec || spec === "this") return el;
		if (spec.startsWith("closest ")) return el.closest(spec.slice(8));
		return document.querySelector(spec);
	}

	function swap(t, how, html) {
		const tpl = document.createElement("template");
		tpl.innerHTML = html;
		const nodes = Array.from(tpl.content.childNodes);
		switch (how || "outerHTML") {
		case "innerHTML": t.replaceChildren(...nodes); break;
		case "afterend": t.after(...nodes); break;
		case "beforeend": t.append(...nodes); break;
		default: t.replaceWith(...nodes);
		}
		for (const n of nodes) {
			if (n.nodeType !== 1) continue;
			const f = n.matches("[autofocus]") ? n : n.querySelector("[autofocus]");
			if (f) { f.focus(); break; }
		}
		return nodes;
	}

	// fragment fetches url and swaps the answer in for el's target.
	async function fragment(el, method, url, body) {
		const t = target(el, el.dataset.target);
		el.classList.add("busy");
		let resp;
		try {
			resp = await fetch(url, { method, body, headers: { "X-Fragment": "1" } });
		} catch (err) {
			el.classList.remove("busy");
			window.forkyouBar && window.forkyouBar("Request failed: " + err);
			return;
		}
		el.classList.remove("busy");
		const text = await resp.text();
		if (!resp.ok) {
			window.forkyouBar && window.forkyouBar(text || resp.statusText);
			return;
		}
		if (resp.headers.get("X-Refresh")) { window.location.reload(); return; }
		const drafts = resp.headers.get("X-Drafts");
		if (drafts !== null) setDraftCount(+drafts);
		if (t) swap(t, el.dataset.swap, text);
		document.dispatchEvent(new CustomEvent("fragment"));
	}
	window.forkyouFragment = fragment;

	// setDraftCount keeps the Reply buttons saying how many drafts there are.
	function setDraftCount(n) {
		const s = n === 1 ? "1 draft" : n + " drafts";
		document.querySelectorAll("#replybutton").forEach((b) => {
			b.textContent = n ? "Reply (" + s + ")" : "Reply";
		});
	}

	document.addEventListener("click", function (e) {
		const rm = e.target.closest("[data-remove]");
		if (rm) {
			e.preventDefault();
			const t = target(rm, rm.dataset.remove);
			if (t) t.remove();
			return;
		}
		const el = e.target.closest("button[data-get], button[data-post], a[data-get]");
		if (!el || el.closest("form[data-post]") && el.type === "submit") return;
		e.preventDefault();
		if (el.dataset.get) fragment(el, "GET", el.dataset.get);
		else fragment(el, "POST", el.dataset.post, new FormData());
	});

	document.addEventListener("submit", function (e) {
		const form = e.target.closest("form[data-post]");
		if (!form) return;
		e.preventDefault();
		fragment(form, "POST", form.dataset.post, new FormData(form));
	});

	// The review message is saved as it is typed, so a reload loses nothing.
	let saveTimer = null;
	document.addEventListener("input", function (e) {
		const ta = e.target.closest("textarea[data-save]");
		if (!ta) return;
		clearTimeout(saveTimer);
		saveTimer = setTimeout(() => {
			const body = new FormData();
			body.append("message", ta.value);
			fetch(ta.dataset.save, { method: "POST", body, headers: { "X-Fragment": "1" } });
		}, 800);
	});
})();
