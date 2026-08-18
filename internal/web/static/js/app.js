/* Doot client. Deliberately small: server-rendered HTML plus htmx, with SSE for
   live updates. No framework, no build step. */
(function () {
  'use strict';

  /* ---------------------------------------------------------------- SSE */

  /* The stream is resumable. Every event carries the events-table id, so a
     reconnect sends Last-Event-ID and the server replays the gap. This is the
     normal case on a phone: locking the screen kills the connection. */
  var Stream = {
    source: null,
    lastId: 0,

    start: function () {
      if (this.source) return;

      var header = document.querySelector('.chat-header');
      if (header && this.lastId === 0) {
        this.lastId = parseInt(header.dataset.lastEvent || '0', 10) || 0;
      }

      /* EventSource cannot set headers, so the resume point is passed as a
         query parameter on the initial connect. The browser sends the
         Last-Event-ID header automatically on its own reconnects. */
      var url = '/events' + (this.lastId ? '?lastEventId=' + this.lastId : '');
      var es = new EventSource(url, { withCredentials: true });
      this.source = es;

      es.onopen = function () { Net.setOffline(false); };

      es.onmessage = function (e) { Stream.track(e); };

      es.onerror = function () {
        /* EventSource retries on its own; surface the state and let it. */
        if (es.readyState === EventSource.CLOSED) {
          Stream.source = null;
          setTimeout(function () { Stream.start(); }, 3000);
        }
        Net.setOffline(true);
      };

      [
        'run.status', 'message.append', 'message.delta', 'tool.start', 'tool.end',
        'diff', 'plan.proposed', 'plan.updated', 'task.updated', 'screenshot',
        'sandbox.state', 'usage', 'epoch.changed'
      ].forEach(function (type) {
        es.addEventListener(type, function (e) {
          Stream.track(e);
          Stream.dispatch(type, e);
        });
      });

      /* The server sends this when the requested resume point has been pruned.
         Replaying an incomplete range would silently drop events, so reload. */
      es.addEventListener('reload', function () {
        window.location.reload();
      });
    },

    track: function (e) {
      if (e.lastEventId) {
        var id = parseInt(e.lastEventId, 10);
        if (!isNaN(id) && id > Stream.lastId) Stream.lastId = id;
      }
    },

    dispatch: function (type, e) {
      var data = e.data;
      switch (type) {
        case 'run.status':
          UI.setRunStatus(data);
          break;
        case 'usage':
          UI.setUsage(data);
          break;
        case 'sandbox.state':
          UI.setSandboxState(data);
          break;
        case 'message.append':
          UI.appendMessage(data);
          break;
        case 'message.delta':
          UI.appendDelta(data);
          break;
        case 'tool.start':
          UI.toolStart(data);
          break;
        case 'tool.end':
          UI.toolEnd(data);
          break;
        case 'plan.proposed':
        case 'plan.updated':
        case 'task.updated':
          UI.planChanged();
          break;
        case 'screenshot':
          UI.addScreenshot(data);
          break;
        case 'preview':
          UI.setPreview(data);
          break;
        case 'epoch.changed':
          UI.epochChanged();
          break;
        default:
          /* Handlers for the rendered-fragment events land here as the agent
             loop is built out. */
          break;
      }
    },

    stop: function () {
      if (this.source) {
        this.source.close();
        this.source = null;
      }
    }
  };

  /* ---------------------------------------------------------------- network */

  var Net = {
    setOffline: function (offline) {
      var banner = document.getElementById('offline-banner');
      if (banner) banner.hidden = !offline;
    }
  };

  window.addEventListener('online', function () { Net.setOffline(false); Stream.start(); });
  window.addEventListener('offline', function () { Net.setOffline(true); });

  /* Mobile browsers suspend background connections. Do not trust that the
     stream survived being backgrounded: tear it down and reconnect, which
     replays anything missed. */
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible') {
      Stream.stop();
      Stream.start();
    }
  });

  /* ---------------------------------------------------------------- UI */

  var UI = {
    setRunStatus: function (payload) {
      var el = document.getElementById('run-status');
      if (!el) return;

      var status = payload;
      try {
        var parsed = JSON.parse(payload);
        if (parsed && parsed.status) status = parsed.status;
      } catch (_) { /* plain string payload */ }

      el.textContent = status;
      el.className = 'pill pill-' + status;

      /* Only a genuinely running run gets Pause. A run parked on a question is
         waiting for input, so Send must stay available — swapping it for Pause
         would leave no way to answer. */
      var running = status === 'running';
      var send = document.getElementById('send-btn');
      var pauseForm = document.getElementById('pause-form');
      if (send) send.hidden = running;
      if (pauseForm) pauseForm.hidden = !running;

      /* Settling into a state that changes the available controls: let the
         server re-render rather than reconstructing it here. */
      if (['done', 'error', 'awaiting_approval', 'awaiting_human', 'paused',
           'cancelled', 'interrupted'].indexOf(status) !== -1) {
        UI._streaming = null;
        UI.scheduleReload(900);
      }
    },

    setUsage: function (payload) {
      var data;
      try { data = JSON.parse(payload); } catch (_) { return; }

      var cost = document.getElementById('usage-cost');
      var ctx = document.getElementById('usage-context');
      if (cost && typeof data.cost_usd === 'number') {
        cost.textContent = '$' + data.cost_usd.toFixed(2);
      }
      if (ctx && typeof data.context_pct === 'number') {
        ctx.textContent = Math.round(data.context_pct) + '%';
      }
    },

    /* Sandbox transitions are slow (snapshot pulls, archive restores), so the
       state is streamed rather than polled. Reaching a settled state reloads the
       screen once, because which controls apply is decided server-side. */
    setSandboxState: function (payload) {
      var data;
      try { data = JSON.parse(payload); } catch (_) { return; }

      var el = document.getElementById('sandbox-state');
      if (el && data.state) {
        el.textContent = data.state;
        el.className = 'pill pill-' + data.state;
      }

      if (data.message) UI.note(data.message);

      var settled = ['started', 'stopped', 'error', ''].indexOf(data.state) !== -1;
      var onSandboxScreen = window.location.pathname === '/project' ||
                            window.location.pathname === '/desktop';

      if (settled && onSandboxScreen && !UI._reloading) {
        UI._reloading = true;
        /* Small delay so the final message is readable before the reload. */
        setTimeout(function () { window.location.reload(); }, 900);
      }
    },

    /* ---------------------------------------------------------- timeline */

    timeline: function () { return document.getElementById('timeline'); },

    /* The streaming assistant bubble. Deltas accumulate into this element, and
       it is retired once the finished message arrives. */
    _streaming: null,

    appendMessage: function (payload) {
      var data;
      try { data = JSON.parse(payload); } catch (_) { return; }

      var timeline = UI.timeline();
      if (!timeline || !data.content) return;

      /* Drop the placeholder the first time anything real arrives. */
      var empty = timeline.querySelector('.empty-inline');
      if (empty) empty.remove();

      /* A completed message supersedes whatever was streaming. */
      if (data.role === 'assistant' && UI._streaming) {
        UI._streaming.remove();
        UI._streaming = null;
      }

      var article = document.createElement('article');
      article.className = 'msg msg-' + (data.role || 'assistant');

      var body = document.createElement('div');
      body.className = 'msg-body';
      body.textContent = data.content;
      article.appendChild(body);

      timeline.appendChild(article);
      UI.scrollTimelineToEnd();
    },

    appendDelta: function (payload) {
      var data;
      try { data = JSON.parse(payload); } catch (_) { return; }
      if (!data.delta) return;

      var timeline = UI.timeline();
      if (!timeline) return;

      if (!UI._streaming) {
        var empty = timeline.querySelector('.empty-inline');
        if (empty) empty.remove();

        UI._streaming = document.createElement('article');
        UI._streaming.className = 'msg msg-assistant is-streaming';
        var body = document.createElement('div');
        body.className = 'msg-body';
        UI._streaming.appendChild(body);
        timeline.appendChild(UI._streaming);
      }

      /* textContent, never innerHTML: model output is untrusted input. */
      UI._streaming.querySelector('.msg-body').textContent += data.delta;
      UI.scrollTimelineToEnd();
    },

    toolStart: function (payload) {
      var data;
      try { data = JSON.parse(payload); } catch (_) { return; }

      var host = UI._streaming || UI.timeline();
      if (!host) return;

      var list = host.querySelector('.tool-list');
      if (!list) {
        list = document.createElement('div');
        list.className = 'tool-list';
        host.appendChild(list);
      }

      var row = document.createElement('div');
      row.className = 'tool-row tool-running';
      row.dataset.toolId = data.id || data.name;

      var name = document.createElement('span');
      name.className = 'tool-name';
      name.textContent = data.name;
      row.appendChild(name);

      list.appendChild(row);
      UI.scrollTimelineToEnd();
    },

    toolEnd: function (payload) {
      var data;
      try { data = JSON.parse(payload); } catch (_) { return; }

      var row = document.querySelector('[data-tool-id="' + (data.id || data.name) + '"]');
      if (!row) return;

      row.className = 'tool-row tool-' + (data.status || 'ok');

      if (data.preview) {
        var preview = document.createElement('span');
        preview.className = 'tool-preview';
        preview.textContent = data.preview;
        row.appendChild(preview);
      }
      if (data.duration) {
        var dur = document.createElement('span');
        dur.className = 'tool-dur';
        dur.textContent = data.duration + 'ms';
        row.appendChild(dur);
      }
    },

    addScreenshot: function (payload) {
      var data;
      try { data = JSON.parse(payload); } catch (_) { return; }
      if (!data.key) return;

      var host = UI._streaming || UI.timeline();
      if (!host) return;

      var strip = host.querySelector('.screenshot-strip');
      if (!strip) {
        strip = document.createElement('div');
        strip.className = 'screenshot-strip';
        host.appendChild(strip);
      }

      var link = document.createElement('a');
      link.href = data.key;
      link.target = '_blank';
      link.rel = 'noopener';

      var img = document.createElement('img');
      img.src = data.key;
      img.alt = 'Verification step ' + (data.step || '');
      img.loading = 'lazy';
      link.appendChild(img);

      strip.appendChild(link);
      UI.scrollTimelineToEnd();
    },

    setPreview: function (payload) {
      var data;
      try { data = JSON.parse(payload); } catch (_) { return; }

      var chip = document.getElementById('preview-chip');
      if (!chip || !data.url) return;
      chip.href = data.url;
      chip.hidden = false;
      chip.textContent = 'Preview :' + (data.port || '');
    },

    /* Plan and task state decide which controls apply, and that is worked out
       server-side. Reloading is simpler and cannot drift from the server's view. */
    planChanged: function () { UI.scheduleReload(600); },
    epochChanged: function () { UI.scheduleReload(400); },

    scheduleReload: function (delay) {
      if (UI._reloading) return;
      UI._reloading = true;
      setTimeout(function () { window.location.reload(); }, delay || 500);
    },

    /* Transient status line. Deliberately a single replaceable element rather
       than stacking toasts. */
    note: function (text) {
      var el = document.getElementById('live-note');
      if (!el) {
        el = document.createElement('div');
        el.id = 'live-note';
        el.className = 'banner banner-nag';
        document.body.insertBefore(el, document.body.firstChild);
      }
      el.textContent = text;
    },

    scrollTimelineToEnd: function () {
      var timeline = document.getElementById('timeline');
      if (timeline) window.scrollTo(0, document.body.scrollHeight);
    }
  };

  /* Read-only desktop by default; the shield only lifts on explicit request. */
  var takeControl = document.getElementById('take-control');
  if (takeControl) {
    takeControl.addEventListener('change', function () {
      var frame = document.querySelector('.vnc-frame');
      if (frame) frame.classList.toggle('is-interactive', takeControl.checked);
    });
  }

  /* Disable submit buttons once clicked. Sandbox operations are single-flight
     server-side, so a double tap would only produce a confusing error. */
  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (!(form instanceof HTMLFormElement)) return;
    var btn = form.querySelector('button[type=submit]');
    if (btn) {
      setTimeout(function () { btn.disabled = true; }, 0);
      setTimeout(function () { btn.disabled = false; }, 8000);
    }
  });

  /* Dismissible banners. */
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-dismiss]');
    if (!btn) return;
    var target = document.querySelector(btn.dataset.dismiss);
    if (target) target.remove();
  });

  /* Confirm destructive actions. Each one confirms individually; there is no
     blanket "are you sure" for a whole screen. */
  document.addEventListener('click', function (e) {
    var el = e.target.closest('[data-confirm]');
    if (!el) return;
    if (!window.confirm(el.dataset.confirm)) {
      e.preventDefault();
      e.stopPropagation();
    }
  });

  /* Close the composer menu when tapping outside it. */
  document.addEventListener('click', function (e) {
    document.querySelectorAll('details.composer-menu[open]').forEach(function (d) {
      if (!d.contains(e.target)) d.removeAttribute('open');
    });
  });

  /* Auto-size the composer, capped by CSS max-height. */
  var input = document.getElementById('composer-input');
  if (input) {
    var resize = function () {
      input.style.height = 'auto';
      input.style.height = input.scrollHeight + 'px';
    };
    input.addEventListener('input', resize);

    /* Enter sends on a physical keyboard; Shift+Enter makes a newline. On
       touch devices Enter must insert a newline, since the on-screen keyboard
       has no other way to do it. */
    input.addEventListener('keydown', function (e) {
      var touch = window.matchMedia('(hover: none)').matches;
      if (e.key === 'Enter' && !e.shiftKey && !touch) {
        e.preventDefault();
        var form = document.getElementById('composer');
        if (form) form.requestSubmit();
      }
    });
  }

  /* ---------------------------------------------------------------- PWA */

  if ('serviceWorker' in navigator) {
    window.addEventListener('load', function () {
      navigator.serviceWorker.register('/sw.js').catch(function (err) {
        console.warn('service worker registration failed', err);
      });
    });
  }

  /* ---------------------------------------------------------------- boot */

  UI.scrollTimelineToEnd();
  if (document.querySelector('.chat-header') || document.querySelector('.tabbar')) {
    Stream.start();
  }

  window.Doot = { Stream: Stream, UI: UI };
})();
