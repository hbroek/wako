(function () {
  'use strict';
  var body = document.body, base = body.dataset.base || '', space = body.dataset.space || '';
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };

  // Changes made with the browser session carry the anti-forgery token the
  // server put in a cookie, which another site cannot read.
  function csrfToken() {
    var m = document.cookie.match(/(?:^|; )wako_csrf=([^;]*)/);
    return m ? decodeURIComponent(m[1]) : '';
  }
  function api(method, url, opts) {
    opts = opts || {};
    var headers = opts.headers || {};
    if (method !== 'GET' && method !== 'HEAD') { var t = csrfToken(); if (t) headers['X-CSRF-Token'] = t; }
    return fetch(url, { method: method, headers: headers, body: opts.body, credentials: 'same-origin' });
  }
  function json(method, url, data) {
    return api(method, url, { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(data) });
  }
  function say(text, err) {
    var m = $('#msg'); if (!m) return;
    m.textContent = text; m.className = err ? 'err' : '';
  }
  function reason(r) {
    return r.text().then(function (t) { return (t || r.statusText).trim(); });
  }

  // ---- theme and navigation drawer ----
  $('#theme').addEventListener('click', function () {
    var root = document.documentElement;
    var dark = root.dataset.theme ? root.dataset.theme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
    root.dataset.theme = dark ? 'light' : 'dark';
    try { localStorage.setItem('theme', root.dataset.theme); } catch (e) {}
  });
  var toggle = $('#navtoggle');
  // Small screens slide the tree in as a drawer; wider ones hide it for good (remembered).
  function toggleNav() {
    if (matchMedia('(max-width:800px)').matches) { body.classList.toggle('nav-open'); return; }
    var hidden = body.classList.toggle('nav-hidden');
    try { localStorage.setItem('nav-hidden', hidden ? '1' : ''); } catch (e) {}
  }
  try { if (localStorage.getItem('nav-hidden')) body.classList.add('nav-hidden'); } catch (e) {}
  if (toggle) toggle.addEventListener('click', toggleNav);

  // ---- tree: expand/collapse and drag and drop ----
  var nav = $('#nav');
  if (nav) {
    nav.addEventListener('click', function (e) {
      var t = e.target.closest('.tog');
      if (t) t.closest('li').classList.toggle('open');
    });
    if (nav.dataset.writable === 'true') enableDnD(nav);
  }

  function pageURL(path) { return base + '/' + space + '/' + path; }
  function parentOf(path) { var i = path.lastIndexOf('/'); return i < 0 ? '' : path.slice(0, i); }
  function slugOf(path) { return path.slice(path.lastIndexOf('/') + 1); }

  function enableDnD(root) {
    var dragged = null;
    function zone(row, e) {
      var r = row.getBoundingClientRect(), y = (e.clientY - r.top) / r.height;
      return y < 0.25 ? 'before' : y > 0.75 ? 'after' : 'into';
    }
    function clear() { $$('.drop-into,.drop-before,.drop-after', root).forEach(function (el) { el.classList.remove('drop-into', 'drop-before', 'drop-after'); }); }
    root.addEventListener('dragstart', function (e) {
      var li = e.target.closest && e.target.closest('li[data-path]');
      if (!li) return;
      dragged = li; li.classList.add('dragging');
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', li.dataset.path);
      e.stopPropagation();
    });
    root.addEventListener('dragend', function () { if (dragged) dragged.classList.remove('dragging'); dragged = null; clear(); });
    root.addEventListener('dragover', function (e) {
      var row = e.target.closest('.row');
      if (!dragged || !row) return;
      var li = row.parentElement;
      if (li === dragged || dragged.contains(li)) return;
      e.preventDefault(); clear();
      row.classList.add('drop-' + zone(row, e));
    });
    root.addEventListener('drop', function (e) {
      var row = e.target.closest('.row');
      if (!dragged || !row) return;
      var li = row.parentElement;
      if (li === dragged || dragged.contains(li)) return;
      e.preventDefault();
      var z = zone(row, e), src = dragged.dataset.path, target = li.dataset.path;
      clear();
      drop(src, target, z).then(function () { location.reload(); }, function (err) { alert(err.message); });
    });
  }

  // Siblings of li (by slug) in DOM order.
  function siblingSlugs(li) {
    return $$(':scope > li', li.parentElement).map(function (s) { return slugOf(s.dataset.path); });
  }

  function drop(src, target, z) {
    var srcSlug = slugOf(src);
    if (z === 'into') {
      return move(src, target + '/' + srcSlug);
    }
    var targetLi = $('li[data-path="' + CSS.escape(target) + '"]', nav);
    var parent = parentOf(target);
    var order = siblingSlugs(targetLi).filter(function (s) { return s !== srcSlug; });
    var at = order.indexOf(slugOf(target)) + (z === 'after' ? 1 : 0);
    order.splice(at, 0, srcSlug);
    var step = parentOf(src) === parent ? Promise.resolve() : move(src, (parent ? parent + '/' : '') + srcSlug);
    return step.then(function () { return reorder(parent, order); });
  }
  function move(from, to) {
    return api('POST', pageURL(from) + '?move=' + encodeURIComponent(to)).then(function (r) {
      if (!r.ok) return reason(r).then(function (m) { throw new Error('Move failed: ' + m); });
    });
  }
  function reorder(parent, order) {
    return json('POST', base + '/' + space + '/' + parent + '?reorder', { order: order }).then(function (r) {
      if (!r.ok) return reason(r).then(function (m) { throw new Error('Reorder failed: ' + m); });
    });
  }

  // ---- page actions: rename/move and delete dialogs ----
  $$('[data-dialog]').forEach(function (b) {
    b.addEventListener('click', function () { var d = document.getElementById(b.dataset.dialog); if (d && d.showModal) d.showModal(); });
  });
  var mv = $('#move');
  if (mv) mv.addEventListener('close', function () {
    if (mv.returnValue !== 'ok') return;
    var path = location.pathname.slice((base + '/' + space + '/').length).replace(/\.md$/, '').replace(/\/$/, '');
    move(decodeURIComponent(path), $('#move-to').value.replace(/^\/+|\/+$/g, '')).then(function () {
      location.href = pageURL($('#move-to').value.replace(/^\/+|\/+$/g, ''));
    }, function (e) { alert(e.message); });
  });
  var del = $('#delete');
  if (del) del.addEventListener('close', function () {
    if (del.returnValue !== 'ok') return;
    var rec = $('#del-rec');
    api('DELETE', del.dataset.url + (rec && rec.checked ? '?recursive=true' : ''), { headers: { 'If-Match': '"v' + del.dataset.version + '"' } }).then(function (r) {
      if (r.ok) location.href = del.dataset.parent;
      else reason(r).then(function (m) { alert('Delete failed: ' + m); });
    });
  });

  // ---- sign in, account, users, members ----
  function failure(r) {
    return r.text().then(function (t) { try { return JSON.parse(t).error || t; } catch (e) { return (t || r.statusText).trim(); } });
  }
  function el(tag, text, cls) { var e = document.createElement(tag); if (text) e.textContent = text; if (cls) e.className = cls; return e; }
  function button(label, fn, cls) { var b = el('button', label, cls); b.type = 'button'; b.addEventListener('click', fn); return b; }

  var lo = $('#logout');
  if (lo) lo.addEventListener('click', function () {
    api('POST', base + '/_api/logout').then(function () { location.href = base + '/'; });
  });
  var lf = $('#login');
  if (lf) lf.addEventListener('submit', function (e) {
    e.preventDefault();
    json('POST', base + '/_api/login', { name: lf.name.value.trim(), password: lf.password.value }).then(function (r) {
      if (r.ok) location.href = lf.dataset.next || base + '/'; else failure(r).then(function (m) { say(m, true); });
    });
  });
  var pf = $('#pw');
  if (pf) pf.addEventListener('submit', function (e) {
    e.preventDefault();
    json('PUT', base + '/_api/me', { current_password: pf.current.value, new_password: pf.next.value }).then(function (r) {
      if (r.ok) { pf.reset(); say('Password changed. Other sessions were signed out.'); } else failure(r).then(function (m) { say(m, true); });
    });
  });

  var tl = $('#tok-list');
  if (tl) {
    var loadTokens = function () {
      api('GET', base + '/_api/tokens').then(function (r) { return r.ok ? r.json() : []; }).then(function (list) {
        tl.textContent = '';
        if (!list.length) tl.appendChild(el('li', 'No tokens yet.', 'hint'));
        list.forEach(function (t) {
          var li = el('li'); li.appendChild(el('span', t.name));
          li.appendChild(el('span', 'created ' + t.created.slice(0, 10) + (t.suggest_only ? ', can only suggest' : ''), 'hint'));
          li.appendChild(button('Revoke', function () {
            api('DELETE', base + '/_api/tokens/' + t.id).then(loadTokens);
          }, 'danger'));
          tl.appendChild(li);
        });
      });
    };
    loadTokens();
    $('#tok-new').addEventListener('submit', function (e) {
      e.preventDefault();
      var f = e.target;
      json('POST', base + '/_api/tokens', { name: f.name.value.trim(), suggest_only: !!(f.suggest && f.suggest.checked) }).then(function (r) {
        if (!r.ok) return failure(r).then(function (m) { alert(m); });
        return r.json().then(function (t) {
          var box = $('#tok-secret'); $('code', box).textContent = t.token; box.hidden = false; f.reset(); loadTokens();
        });
      });
    });
  }

  var ut = $('#users');
  if (ut) {
    var body = $('tbody', ut), pwd = $('#pw-dialog'), pwFor = null;
    var loadUsers = function () {
      api('GET', base + '/_api/users').then(function (r) { return r.ok ? r.json() : []; }).then(function (list) {
        body.textContent = '';
        list.forEach(function (u) {
          var tr = el('tr'), name = el('td', u.name), adm = el('td'), act = el('td');
          var cb = document.createElement('input'); cb.type = 'checkbox'; cb.checked = u.is_admin;
          cb.addEventListener('change', function () {
            json('PUT', base + '/_api/users/' + encodeURIComponent(u.name), { is_admin: cb.checked }).then(function (r) {
              if (!r.ok) failure(r).then(function (m) { alert(m); loadUsers(); });
            });
          });
          adm.appendChild(cb);
          act.appendChild(button('Set password', function () { pwFor = u.name; $('#pw-user').textContent = u.name; $('#pw-new').value = ''; pwd.showModal(); }));
          act.appendChild(document.createTextNode(' '));
          act.appendChild(button('Delete', function () {
            if (!confirm('Delete ' + u.name + '? Their sessions and tokens go too.')) return;
            api('DELETE', base + '/_api/users/' + encodeURIComponent(u.name)).then(function (r) {
              if (r.ok) loadUsers(); else failure(r).then(function (m) { alert(m); });
            });
          }, 'danger'));
          tr.appendChild(name); tr.appendChild(adm); tr.appendChild(act); body.appendChild(tr);
        });
      });
    };
    loadUsers();
    pwd.addEventListener('close', function () {
      if (pwd.returnValue !== 'ok' || !pwFor) return;
      json('PUT', base + '/_api/users/' + encodeURIComponent(pwFor), { password: $('#pw-new').value }).then(function (r) {
        if (!r.ok) failure(r).then(function (m) { alert(m); });
      });
    });
    $('#user-new').addEventListener('submit', function (e) {
      e.preventDefault();
      var f = e.target;
      json('POST', base + '/_api/users', { name: f.name.value.trim(), password: f.password.value, is_admin: f.admin.checked }).then(function (r) {
        if (r.ok) { f.reset(); say('User added.'); loadUsers(); } else failure(r).then(function (m) { say(m, true); });
      });
    });
  }

  var imp = $('#import-form');
  if (imp) imp.addEventListener('submit', function (e) {
    e.preventDefault();
    var m = $('#msg3'), out = $('#import-report'), file = imp.file.files[0];
    if (!file) return;
    m.textContent = 'Importing…'; m.className = ''; out.hidden = true;
    var fd = new FormData(); fd.append('file', file);
    api('POST', imp.dataset.url + '?conflict=' + encodeURIComponent(imp.conflict.value), { body: fd }).then(function (r) {
      return r.text().then(function (t) { var j = {}; try { j = JSON.parse(t); } catch (x) { j = { error: t }; } return { ok: r.ok, j: j }; });
    }).then(function (res) {
      var rep = res.j.report || (res.ok ? res.j : null), lines = [];
      if (rep) {
        lines.push(rep.pages_created + ' pages created, ' + rep.pages_updated + ' updated, ' + rep.pages_skipped + ' skipped' +
          (rep.placeholders ? ', ' + rep.placeholders + ' empty parent pages added' : '') + '.');
        lines.push(rep.images_added + ' images added, ' + rep.images_existing + ' already in the library.');
        (rep.warnings || []).forEach(function (w) { lines.push('• ' + w); });
        out.textContent = lines.join('\n'); out.hidden = false;
      }
      if (res.ok) { m.textContent = 'Done.'; imp.reset(); } else { m.textContent = res.j.error || 'Import failed'; m.className = 'err'; }
    }, function () { m.textContent = 'Import failed: network error'; m.className = 'err'; });
  });

  var mt = $('#members');
  if (mt) {
    var loadMembers = function () {
      api('GET', mt.dataset.url, { headers: { 'Accept': 'application/json' } }).then(function (r) { return r.ok ? r.json() : []; }).then(function (list) {
        mt.textContent = '';
        if (!list.length) { var tr0 = el('tr'); tr0.appendChild(el('td', 'No members yet.', 'hint')); mt.appendChild(tr0); }
        list.forEach(function (m) {
          var tr = el('tr'), sel = document.createElement('select');
          ['viewer', 'suggester', 'editor', 'admin'].forEach(function (r) { var o = el('option', r); o.selected = r === m.role; sel.appendChild(o); });
          sel.addEventListener('change', function () {
            json('PUT', mt.dataset.url + '/' + encodeURIComponent(m.name), { role: sel.value }).then(function (r) {
              if (!r.ok) failure(r).then(function (t) { alert(t); loadMembers(); });
            });
          });
          var c1 = el('td', m.name), c2 = el('td'), c3 = el('td'); c2.appendChild(sel);
          c3.appendChild(button('Remove', function () {
            api('DELETE', mt.dataset.url + '/' + encodeURIComponent(m.name)).then(loadMembers);
          }, 'danger'));
          tr.appendChild(c1); tr.appendChild(c2); tr.appendChild(c3); mt.appendChild(tr);
        });
      });
    };
    loadMembers();
    $('#member-new').addEventListener('submit', function (e) {
      e.preventDefault();
      var f = e.target, m = $('#msg2');
      json('PUT', f.dataset.url + '/' + encodeURIComponent(f.user.value.trim()), { role: f.role.value }).then(function (r) {
        if (r.ok) { f.user.value = ''; m.textContent = ''; loadMembers(); } else failure(r).then(function (t) { m.textContent = t; m.className = 'err'; });
      });
    });
  }

  // ---- ask the wiki, and draft pages ----
  var ak = $('#ask');
  if (ak && $('#ask-form')) {
    var A = ak.dataset.api, askForm = $('#ask-form'), draftForm = $('#draft-form');
    var err = function (r) { return r.text().then(function (t) { try { return JSON.parse(t).error || t; } catch (e) { return (t || r.statusText).trim(); } }); };
    var fill = function (sel, slugs, want) {
      sel.textContent = '';
      if (sel.id === 'ask-space' && slugs.length > 1) { var all = el('option', 'all spaces'); all.value = ''; sel.appendChild(all); }
      slugs.forEach(function (s) { var o = el('option', s); o.value = s; if (s === want) o.selected = true; sel.appendChild(o); });
    };
    var list = function (box, sources) {
      var ol = $('ol', box); ol.textContent = '';
      sources.forEach(function (s) {
        var li = el('li'), a = el('a', s.title); a.href = s.url; li.appendChild(a); li.appendChild(el('span', ' /' + s.path, 'hint')); ol.appendChild(li);
      });
      box.hidden = !sources.length;
    };
    api('GET', A, { headers: { 'Accept': 'application/json' } }).then(function (r) { return r.json(); }).then(function (st) {
      $('#ask-model').textContent = st.model ? 'Model: ' + st.model : '';
      fill($('#ask-space'), st.spaces, ak.dataset.space);
      fill($('#draft-space'), st.draft, ak.dataset.space);
      if (ak.dataset.path) { $('#draft-path').value = ak.dataset.path; $('#draft-save-path').value = ak.dataset.path; }
      if (!st.spaces.length) $('#ask-msg').textContent = 'No space you can read has the AI features switched on.';
      if (!st.draft.length) $('#draft-msg').textContent = 'You cannot edit or suggest changes in any space that has the AI features switched on.';
    });
    $$('[data-tab]').forEach(function (b) {
      b.addEventListener('click', function () {
        $$('[data-tab]').forEach(function (x) { x.classList.toggle('on', x === b); });
        $('#tab-ask').hidden = b.dataset.tab !== 'ask'; $('#tab-draft').hidden = b.dataset.tab !== 'draft';
      });
    });
    askForm.addEventListener('submit', function (e) {
      e.preventDefault();
      var m = $('#ask-msg'); m.textContent = 'Reading the wiki…'; m.className = '';
      $('#ask-answer').hidden = true; $('#ask-sources').hidden = true;
      json('POST', A + '/ask', { question: askForm.question.value.trim(), space: askForm.space.value }).then(function (r) {
        if (!r.ok) return err(r).then(function (t) { m.textContent = t; m.className = 'err'; });
        return r.json().then(function (res) {
          m.textContent = '';
          var box = $('#ask-answer'); box.innerHTML = res.html; box.hidden = false; // sanitised by the server, without images
          list($('#ask-sources'), res.sources);
        });
      }, function () { m.textContent = 'Network error'; m.className = 'err'; });
    });
    var lastDraft = null;
    draftForm.addEventListener('submit', function (e) {
      e.preventDefault();
      var m = $('#draft-msg'); m.textContent = 'Writing a draft…'; m.className = '';
      $('#draft-result').hidden = true;
      json('POST', A + '/draft', { space: draftForm.space.value, instruction: draftForm.instruction.value.trim(), path: draftForm.path.value.trim() }).then(function (r) {
        if (!r.ok) return err(r).then(function (t) { m.textContent = t; m.className = 'err'; });
        return r.json().then(function (res) {
          m.textContent = ''; lastDraft = { res: res, space: draftForm.space.value, instruction: draftForm.instruction.value.trim() };
          $('#draft-body').value = res.markdown;
          $('#draft-save-path').value = res.path || '';
          $('#draft-note').textContent = (res.exists ? 'This rewrites the existing page /' + res.path + '. ' : '') + (res.truncated ? 'The draft was cut off at the length limit. ' : '') + 'Drafted by ' + res.model + '.';
          list($('#draft-sources'), res.sources);
          $('#draft-result').hidden = false;
        });
      }, function () { m.textContent = 'Network error'; m.className = 'err'; });
    });
    $('#draft-save').addEventListener('click', function () {
      var m = $('#draft-save-msg'), path = $('#draft-save-path').value.trim().replace(/^\/+|\/+$/g, '');
      if (!lastDraft || !path) { m.textContent = 'Enter the page address to save it as.'; m.className = 'err'; return; }
      var h = { 'Content-Type': 'text/markdown', 'X-Wako-Source': 'ai:' + lastDraft.res.model.replace(/[^A-Za-z0-9._-]/g, ''),
                'X-Wako-Note': ('from the instruction: ' + lastDraft.instruction).replace(/[^\x20-\x7e]/g, '').slice(0, 200) };
      if (lastDraft.res.exists && path === lastDraft.res.path) h['If-Match'] = '"v' + lastDraft.res.base_version + '"'; else h['If-None-Match'] = '*';
      m.textContent = 'Saving…'; m.className = '';
      api('PUT', base + '/' + lastDraft.space + '/' + path.split('/').map(encodeURIComponent).join('/'), { headers: h, body: $('#draft-body').value }).then(function (r) {
        if (r.status === 202) { m.textContent = 'Sent as a suggestion. An editor will review it.'; return; }
        if (r.ok) { m.textContent = ''; var a = el('a', 'Saved: open the page'); a.href = r.headers.get('Location') || '#'; m.appendChild(a); return; }
        err(r).then(function (t) {
          m.textContent = r.status === 412 ? 'A page with that address exists already (or it changed). Choose another address.' : t; m.className = 'err';
        });
      }, function () { m.textContent = 'Network error'; m.className = 'err'; });
    });
  }

  // ---- review a suggestion ----
  var pr = $('#proposal');
  if (pr) {
    var decide = function (action, data) {
      say('Working…');
      json('POST', pr.dataset.url + '/' + action, data || {}).then(function (r) {
        if (r.ok) location.href = pr.dataset.back; else reason(r).then(function (m) { say(m, true); });
      }, function () { say('Network error', true); });
    };
    var note = function () { var n = $('#p-note'); return n ? n.value.trim() : ''; };
    var ok = $('#p-accept'), no = $('#p-reject'), wd = $('#p-withdraw');
    if (ok) ok.addEventListener('click', function () {
      var data = { note: note() }, ta = $('#p-body');
      if (ta && ta.value !== ta.defaultValue) data.body = ta.value;
      decide('accept', data);
    });
    if (no) no.addEventListener('click', function () { decide('reject', { note: note() }); });
    if (wd) wd.addEventListener('click', function () { decide('withdraw', {}); });
  }

  // ---- print / save as PDF ----
  $$('[data-print]').forEach(function (b) { b.addEventListener('click', function () { window.print(); }); });

  // ---- restore an earlier revision ----
  $$('[data-restore]').forEach(function (b) {
    b.addEventListener('click', function () {
      if (!confirm('Make revision ' + b.dataset.restore + ' the current text? The current text stays in the history.')) return;
      api('POST', b.dataset.url + '?restore=' + b.dataset.restore).then(function (r) {
        if (r.ok) location.href = b.dataset.url; else failure(r).then(function (m) { alert('Restore failed: ' + m); });
      });
    });
  });

  // ---- backlinks ----
  var bl = $('#backlinks');
  if (bl) api('GET', bl.dataset.url, { headers: { 'Accept': 'application/json' } }).then(function (r) {
    return r.ok ? r.json() : [];
  }).then(function (list) {
    if (!list.length) return;
    var ul = $('ul', bl);
    list.forEach(function (h) {
      var li = document.createElement('li'), a = document.createElement('a');
      a.href = h.url; a.textContent = h.title;
      li.appendChild(a);
      if (h.space !== space) { var s = document.createElement('span'); s.className = 'hint'; s.textContent = ' in ' + h.space_title; li.appendChild(s); }
      ul.appendChild(li);
    });
    bl.hidden = false;
  });

  // ---- search pane (Ctrl+Shift+F) ----
  var pane = $('#search-pane');
  if (pane) {
    var sq = $('#sp-q'), sres = $('#sp-results'), sall = $('#sp-all'), smore = $('#sp-more'), sel = -1, sTimer = null, sSeq = 0;
    var setSel = function (i) {
      var items = $$('li', sres); if (!items.length) { sel = -1; return; }
      sel = (i + items.length) % items.length;
      items.forEach(function (li, k) { li.classList.toggle('sel', k === sel); });
      items[sel].scrollIntoView({ block: 'nearest' });
    };
    var runSearch = function () {
      var q = sq.value.trim(), seq = ++sSeq;
      var scope = pane.dataset.space && !(sall && sall.checked) ? pane.dataset.space : '';
      smore.href = base + '/_ui/search?q=' + encodeURIComponent(q) + (scope ? '&space=' + encodeURIComponent(scope) : '');
      if (!q) { sres.textContent = ''; return; }
      api('GET', pane.dataset.api + '?limit=12&q=' + encodeURIComponent(q) + (scope ? '&space=' + encodeURIComponent(scope) : ''),
        { headers: { 'Accept': 'application/json' } }).then(function (r) { return r.ok ? r.json() : []; }).then(function (hits) {
        if (seq !== sSeq) return; // a newer search is already running
        sres.textContent = '';
        if (!hits.length) { var li = document.createElement('li'); li.className = 'hint'; li.textContent = 'No pages match.'; sres.appendChild(li); sel = -1; return; }
        hits.forEach(function (h) {
          var li = document.createElement('li'), a = document.createElement('a'), sn = document.createElement('div');
          a.href = h.url; a.innerHTML = h.title_html; // escaped by the server, matches wrapped in <mark>
          sn.className = 'snippet'; sn.innerHTML = h.snippet_html;
          var where = document.createElement('span'); where.className = 'hint';
          where.textContent = ' ' + (h.space !== pane.dataset.space || (sall && sall.checked) ? h.space_title + ' / ' : '') + h.path;
          li.appendChild(a); li.appendChild(where); li.appendChild(sn); sres.appendChild(li);
        });
        setSel(0);
      });
    };
    var openPane = function () { pane.showModal(); sq.select(); if (sq.value) runSearch(); };
    sq.addEventListener('input', function () { clearTimeout(sTimer); sTimer = setTimeout(runSearch, 150); });
    if (sall) sall.addEventListener('change', runSearch);
    sq.addEventListener('keydown', function (e) {
      if (e.key === 'ArrowDown') { e.preventDefault(); setSel(sel + 1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); setSel(sel - 1); }
      else if (e.key === 'Enter') {
        e.preventDefault();
        var a = sel >= 0 && $$('li a', sres)[sel];
        location.href = a ? a.href : smore.href;
      }
    });
    document.addEventListener('keydown', function (e) {
      if ((e.ctrlKey || e.metaKey) && e.shiftKey && e.code === 'KeyF') { e.preventDefault(); if (!pane.open) openPane(); }
    });
  }

  // ---- keyboard shortcuts on view pages ----
  document.addEventListener('keydown', function (e) {
    var mod = e.ctrlKey || e.metaKey;
    if (mod && e.shiftKey && e.code === 'KeyE') { e.preventDefault(); toggleNav(); return; }
    if (mod && !e.shiftKey && !e.altKey && e.code === 'KeyE') {
      var edit = $('#edit-link');
      if (edit) { e.preventDefault(); edit.click(); }
    }
  });

  // ---- editor ----
  var form = $('#f');
  if (form) initEditor(form);

  function initEditor(f) {
    var src = $('#src'), preview = $('#preview'), panes = $('.panes'), d = f.dataset;
    var mode = d.mode, version = d.version, target = d.url, view = d.view, dirty = false, timer = null;

    function renderPreview() {
      api('POST', d.render + (d.page ? '?page=' + encodeURIComponent(d.page) : ''), { headers: { 'Content-Type': 'text/markdown' }, body: src.value }).then(function (r) {
        return r.ok ? r.text() : ''; }).then(function (h) { preview.innerHTML = h; });
    }
    src.addEventListener('input', function () {
      dirty = true; clearTimeout(timer); timer = setTimeout(renderPreview, 250);
    });
    renderPreview();
    $$('[data-pane]').forEach(function (b) {
      b.addEventListener('click', function () { panes.classList.toggle('show-preview', b.dataset.pane === 'preview'); if (b.dataset.pane === 'preview') renderPreview(); });
    });
    window.addEventListener('beforeunload', function (e) { if (dirty) { e.preventDefault(); e.returnValue = ''; } });

    function save() {
      var h = { 'Content-Type': 'text/markdown' }, url = target;
      if (mode === 'edit') h['If-Match'] = '"v' + version + '"';
      else {
        h['If-None-Match'] = '*';
        if (mode === 'create') {
          var slug = f.slug.value.trim();
          if (!slug) { say('Enter a slug first.', true); f.slug.focus(); return; }
          url = target.replace(/\/$/, '') + '/' + slug;
        }
      }
      say('Saving…');
      api('PUT', url, { headers: h, body: src.value }).then(function (r) {
        if (!r.ok) {
          if (r.status === 412 && mode === 'edit') { showConflict(); return; }
          say(r.status === 412 ? 'The page already exists. Choose another slug, or open it and edit that.' : 'Save failed (' + r.status + ')', true);
          return;
        }
        if (r.status === 202) { // a suggestion, not a save: an editor reviews it
          dirty = false; var where = r.headers.get('Location'); if (where) $('#done').href = where;
          say('Sent as a suggestion. An editor will review it.');
          return;
        }
        var et = (r.headers.get('ETag') || '').match(/v(\d+)/);
        version = et ? et[1] : version;
        target = url; view = r.headers.get('Location') || url; mode = 'edit'; dirty = false;
        $('#done').href = view;
        if (f.slug) f.slug.readOnly = true;
        say('Saved.');
      }, function () { say('Save failed: network error', true); });
    }
    f.addEventListener('submit', function (e) { e.preventDefault(); save(); });

    // Another person saved first: show what they changed, and let the author decide.
    var panel = $('#conflict'), theirs = null;
    function showConflict() {
      say('Someone else saved this page first.', true);
      api('GET', target, { headers: { 'Accept': 'text/markdown' } }).then(function (r) {
        var et = (r.headers.get('ETag') || '').match(/v(\d+)/);
        return r.text().then(function (text) { theirs = { text: text, version: et ? et[1] : null }; });
      }).then(function () {
        return json('POST', d.diff, { from: theirs.text, to: src.value });
      }).then(function (r) { return r.json(); }).then(function (res) {
        $('#conflict-diff').innerHTML = res.changed ? res.html : '<p class="hint">The texts are identical; only the version differs.</p>'; // escaped by the server
        panel.hidden = false; panel.scrollIntoView({ block: 'nearest' });
      });
    }
    $('#conflict-mine').addEventListener('click', function () {
      if (theirs && theirs.version) { version = theirs.version; panel.hidden = true; save(); }
    });
    $('#conflict-theirs').addEventListener('click', function () {
      if (!theirs) return;
      src.value = theirs.text; version = theirs.version; dirty = false; panel.hidden = true; say('Loaded their version.'); renderPreview();
    });
    $('#conflict-keep').addEventListener('click', function () { panel.hidden = true; });

    // ---- images: paste, drop or pick a file; it is uploaded and linked ----
    function insertAtCursor(text) {
      var s = src.selectionStart, e = src.selectionEnd;
      src.setRangeText(text, s, e, 'end');
      src.dispatchEvent(new Event('input'));
    }
    function upload(file) {
      if (!file || file.type.indexOf('image/') !== 0) { say('Only image files can be added.', true); return Promise.resolve(); }
      say('Uploading ' + (file.name || 'image') + '…');
      return api('POST', base + '/' + space + '/_assets?name=' + encodeURIComponent(file.name || 'image'), {
        headers: { 'Content-Type': file.type }, body: file
      }).then(function (r) {
        if (!r.ok) return reason(r).then(function (m) { say('Upload failed: ' + m, true); });
        return r.json().then(function (a) { insertImage(a.name); say('Image added: ' + a.name); });
      }, function () { say('Upload failed: network error', true); });
    }
    // Markdown refers to a library image by its bare name; the page binds it on save.
    function insertImage(name) {
      insertAtCursor('![' + name.replace(/\.[^.]+$/, '') + '](' + name + ')\n');
    }
    function files(dt) {
      return Array.prototype.filter.call(dt && dt.files || [], function (f) { return f.type.indexOf('image/') === 0; });
    }
    src.addEventListener('paste', function (e) {
      var imgs = files(e.clipboardData);
      if (!imgs.length) return; // plain text paste
      e.preventDefault(); imgs.forEach(upload);
    });
    src.addEventListener('dragover', function (e) { if (e.dataTransfer && e.dataTransfer.types.indexOf('Files') >= 0) e.preventDefault(); });
    src.addEventListener('drop', function (e) {
      var imgs = files(e.dataTransfer);
      if (!imgs.length) return;
      e.preventDefault(); imgs.forEach(upload);
    });
    var dlg = $('#media'), grid = $('#media-grid'), q = $('#media-q'), pick = $('#pick'), qTimer = null;
    function loadLibrary() {
      api('GET', d.library + '?q=' + encodeURIComponent(q.value), { headers: { 'Accept': 'application/json' } })
        .then(function (r) { return r.ok ? r.json() : []; }).then(function (list) {
          grid.textContent = '';
          if (!list.length) { var p = document.createElement('p'); p.className = 'hint'; p.textContent = 'No images yet. Upload one.'; grid.appendChild(p); }
          list.forEach(function (a) {
            var b = document.createElement('button'); b.type = 'button'; b.className = 'tile'; b.title = a.name;
            var img = document.createElement('img'); img.src = a.url + '?w=240'; img.decoding = 'async'; img.alt = ''; img.loading = 'lazy';
            var c = document.createElement('div'); c.className = 'hint'; c.style.padding = '.3rem .5rem'; c.textContent = a.name;
            b.appendChild(img); b.appendChild(c);
            b.addEventListener('click', function () { insertImage(a.name); dlg.close(); src.focus(); });
            grid.appendChild(b);
          });
        });
    }
    $('#media-btn').addEventListener('click', function () { dlg.showModal(); loadLibrary(); q.focus(); });
    q.addEventListener('input', function () { clearTimeout(qTimer); qTimer = setTimeout(loadLibrary, 200); });
    pick.addEventListener('change', function () {
      Promise.all(Array.prototype.map.call(pick.files, upload)).then(function () { pick.value = ''; dlg.close(); src.focus(); });
    });

    function wrap(mark) {
      var s = src.selectionStart, e = src.selectionEnd, v = src.value, sel = v.slice(s, e);
      src.setRangeText(mark + sel + mark, s, e, 'end');
      if (!sel) src.setSelectionRange(s + mark.length, s + mark.length);
      src.dispatchEvent(new Event('input'));
    }
    function heading(level) {
      var v = src.value, pos = src.selectionStart, start = v.lastIndexOf('\n', pos - 1) + 1;
      var end = v.indexOf('\n', pos); if (end < 0) end = v.length;
      var line = v.slice(start, end).replace(/^#{1,6}\s*/, '');
      src.setRangeText('#'.repeat(level) + ' ' + line, start, end, 'end');
      src.dispatchEvent(new Event('input'));
    }
    document.addEventListener('keydown', function (e) {
      var mod = e.ctrlKey || e.metaKey;
      if (mod && !e.shiftKey && !e.altKey && e.code === 'KeyS') { e.preventDefault(); save(); }
      else if (mod && !e.shiftKey && !e.altKey && e.code === 'KeyB') { e.preventDefault(); wrap('**'); }
      else if (mod && !e.shiftKey && !e.altKey && e.code === 'KeyI') { e.preventDefault(); wrap('*'); }
      else if (mod && e.altKey && /^Digit[123]$/.test(e.code)) { e.preventDefault(); heading(+e.code.slice(5)); }
      else if (mod && e.shiftKey && e.key === 'Enter') { e.preventDefault(); toggleFocus(); }
      else if (e.key === 'Escape' && !document.querySelector('dialog[open]')) {
        if (body.classList.contains('focus')) toggleFocus(); else location.href = view;
      }
    });

    // ---- focus mode: only the editor, no tree, header, preview or assistant ----
    var fbtn = $('#focus-btn');
    function toggleFocus() {
      var on = body.classList.toggle('focus');
      if (on) panes.classList.remove('show-preview');
      fbtn.textContent = on ? 'Exit focus' : 'Focus';
      src.focus();
    }
    fbtn.addEventListener('click', toggleFocus);

    // ---- the assistant under the editor ----
    var asst = $('#assist');
    if (asst) {
      var aq = $('#assist-q'), amsg = $('#assist-msg'), ago = $('#assist-go'), aundo = $('#assist-undo'), trail = [], busy = false;
      var note = function (t, bad) { amsg.textContent = t; amsg.className = bad ? 'err' : ''; };
      var apiErr = function (r) { return r.text().then(function (t) { try { return JSON.parse(t).error || t; } catch (e) { return (t || r.statusText).trim(); } }); };
      // Where the text goes, and what the text becomes. v is the editor's text when the request was sent.
      var place = function (v, s, e, res) {
        var t = res.text, from, to, out;
        if (res.mode === 'replace') {
          from = res.place === 'page' ? 0 : s; to = res.place === 'page' ? v.length : e;
          return { text: v.slice(0, from) + t + v.slice(to), from: from, to: from + t.length };
        }
        var at = res.place === 'cursor' ? e : v.length, b = v.slice(0, at), a = v.slice(at), pre, post;
        if (res.place === 'cursor' && b && a && !/\n$/.test(b) && !/^\n/.test(a) && t.indexOf('\n') < 0) { pre = post = ''; } // a few words inside a line
        else {
          pre = !b ? '' : /\n\n$/.test(b) ? '' : /\n$/.test(b) ? '\n' : '\n\n';
          post = !a ? '\n' : /^\n\n/.test(a) ? '' : /^\n/.test(a) ? '\n' : '\n\n';
        }
        from = b.length + pre.length;
        return { text: b + pre + t + post + a, from: from, to: from + t.length };
      };
      var show = function (text, from, to) {
        src.value = text; src.blur(); src.setSelectionRange(from, to); src.focus(); // blur first: it makes the browser scroll to the selection
        src.dispatchEvent(new Event('input'));
      };
      var undo = function () {
        var h = trail.pop(); if (!h) return;
        show(h.before, h.s, h.e); aundo.disabled = !trail.length; note('Undone.');
      };
      aundo.addEventListener('click', undo);
      src.addEventListener('keydown', function (e) { // Ctrl+Z takes back the assistant's change while nothing was typed after it
        if ((e.ctrlKey || e.metaKey) && !e.shiftKey && !e.altKey && e.code === 'KeyZ' && trail.length && trail[trail.length - 1].after === src.value) { e.preventDefault(); undo(); }
      });
      aq.addEventListener('keydown', function (e) { // the editor's shortcuts act on the text, not on this box
        if (e.key === 'Escape') { e.stopPropagation(); aq.blur(); src.focus(); }
        else if (!((e.ctrlKey || e.metaKey) && e.code === 'KeyS')) e.stopPropagation();
      });
      document.addEventListener('keydown', function (e) {
        if ((e.ctrlKey || e.metaKey) && !e.shiftKey && !e.altKey && e.code === 'Slash') { e.preventDefault(); aq.focus(); }
      });
      asst.addEventListener('submit', function (ev) {
        ev.preventDefault();
        var q = aq.value.trim(); if (!q || busy) return;
        var v = src.value, s = src.selectionStart, e = src.selectionEnd; // a textarea keeps its selection while it is not focused
        busy = true; ago.disabled = true; note('Working…');
        json('POST', asst.dataset.api, { space: asst.dataset.space, instruction: q, before: v.slice(0, s), selected: v.slice(s, e), after: v.slice(e) }).then(function (r) {
          if (!r.ok) return apiErr(r).then(function (t) { note(t, true); });
          return r.json().then(function (res) {
            if (src.value !== v) { note('The text changed while the assistant was working, so its answer was not used. Try again.', true); return; }
            var p = place(v, s, e, res);
            trail.push({ before: v, s: s, e: e, after: p.text }); if (trail.length > 20) trail.shift();
            show(p.text, p.from, p.to); aundo.disabled = false; aq.value = '';
            note((res.mode === 'replace' ? 'Changed' : 'Added') + ' by ' + res.model + (res.truncated ? ', but cut off at the length limit' : '') + '. Not saved yet; Undo takes it back.');
          });
        }, function () { note('Network error', true); }).then(function () { busy = false; ago.disabled = false; });
      });
    }
    src.focus();
  }

  // ---- media library page ----
  var lib = $('#lib');
  if (lib) {
    var up = $('#lib-upload');
    if (up) up.addEventListener('change', function () {
      Promise.all(Array.prototype.map.call(up.files, function (f) {
        return api('POST', lib.dataset.url + '?name=' + encodeURIComponent(f.name), { headers: { 'Content-Type': f.type }, body: f })
          .then(function (r) { if (!r.ok) return reason(r).then(function (m) { alert(f.name + ': ' + m); }); });
      })).then(function () { location.reload(); });
    });
    var rn = $('#rename'), renaming = null;
    lib.addEventListener('click', function (e) {
      var r = e.target.closest('[data-rename]');
      if (!r || !rn) return;
      renaming = { id: r.closest('.tile').dataset.id, name: r.dataset.name, used: +r.dataset.used };
      $('#rename-to').value = renaming.name;
      $('#rename-note').textContent = renaming.used > 0
        ? 'Used by ' + renaming.used + ' page' + (renaming.used === 1 ? '' : 's') + '. Their Markdown is updated to the new name.'
        : 'No page uses this image yet.';
      rn.showModal(); $('#rename-to').select();
    });
    if (rn) rn.addEventListener('close', function () {
      if (rn.returnValue !== 'ok' || !renaming) return;
      var to = $('#rename-to').value.trim();
      if (!to || to === renaming.name) return;
      api('POST', lib.dataset.url + '/' + renaming.id + '?rename=' + encodeURIComponent(to)).then(function (r) {
        if (r.ok) location.reload(); else reason(r).then(function (m) { alert('Rename failed: ' + m); });
      });
    });
    lib.addEventListener('click', function (e) {
      var b = e.target.closest('[data-del]'); if (!b) return;
      var url = lib.dataset.url + '/' + b.closest('.tile').dataset.id, used = +b.dataset.used;
      if (used > 0 && !confirm('This image is used by ' + used + ' page(s). Delete it anyway? Those pages will show a broken image.')) return;
      api('DELETE', url + (used > 0 ? '?force=true' : '')).then(function (r) {
        if (r.ok) location.reload(); else reason(r).then(function (m) { alert('Delete failed: ' + m); });
      });
    });
  }

  // ---- space forms ----
  function spaceBody(f) {
    var o = { title: f.title.value.trim(), slug: f.slug.value.trim(), public_read: f.public.checked };
    if (f.ai) o.ai_enabled = f.ai.checked;
    return o;
  }
  var nf = $('#space-new');
  if (nf) {
    var touched = false;
    nf.slug.addEventListener('input', function () { touched = true; });
    nf.title.addEventListener('input', function () {
      if (!touched) nf.slug.value = nf.title.value.toLowerCase().trim().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
    });
    nf.addEventListener('submit', function (e) {
      e.preventDefault();
      json('POST', base + '/_api/spaces', spaceBody(nf)).then(function (r) {
        if (r.ok) location.href = r.headers.get('Location'); else reason(r).then(function (m) { say(m, true); });
      });
    });
  }
  var sf = $('#space-settings');
  if (sf) sf.addEventListener('submit', function (e) {
    e.preventDefault();
    json('PUT', base + '/_api/spaces/' + sf.dataset.slug, spaceBody(sf)).then(function (r) {
      if (r.ok) location.href = r.headers.get('Location') + '?settings'; else reason(r).then(function (m) { say(m, true); });
    });
  });
  var sd = $('#space-delete');
  if (sd) sd.addEventListener('submit', function (e) {
    e.preventDefault();
    if (sd.confirm.value !== sd.dataset.slug) { sd.confirm.focus(); return; }
    api('DELETE', base + '/_api/spaces/' + sd.dataset.slug).then(function (r) {
      if (r.ok) location.href = base + '/'; else reason(r).then(function (m) { alert(m); });
    });
  });
})();
