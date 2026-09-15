// The app's only JavaScript. Deliberately small and dependency-free: this
// product is maintained by one non-engineer, and a CDN script tag is a
// third party in the critical path of a page someone opens at 6am.
(function () {
  'use strict';

  // --- service worker -----------------------------------------------------
  // Registered from the root, not from /static/. A worker's default scope is
  // its own directory, so /static/sw.js could only ever control /static/ —
  // which is not enough for the browser to consider the app installable.
  if ('serviceWorker' in navigator) {
    window.addEventListener('load', function () {
      navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(function () {
        // Losing this costs the install prompt, not the app.
      });
    });
  }

  // --- install button -----------------------------------------------------
  var deferredPrompt = null;

  window.addEventListener('beforeinstallprompt', function (event) {
    event.preventDefault();
    deferredPrompt = event;
    var button = document.getElementById('install-app');
    if (button) button.hidden = false;
  });

  window.addEventListener('appinstalled', function () {
    var button = document.getElementById('install-app');
    if (button) button.hidden = true;
    deferredPrompt = null;
  });

  document.addEventListener('click', function (event) {
    var button = event.target.closest ? event.target.closest('#install-app') : null;
    if (!button || !deferredPrompt) return;
    deferredPrompt.prompt();
    deferredPrompt.userChoice.finally(function () {
      deferredPrompt = null;
      button.hidden = true;
    });
  });

  // --- notifications ---
  // Subscribing needs an explicit gesture: a permission prompt fired on page
  // load is the fastest way to get permanently denied.
  async function enableNotifications(button) {
    try {
      const reg = await navigator.serviceWorker.ready;
      const permission = await Notification.requestPermission();
      if (permission !== 'granted') {
        button.textContent = 'Notifications are blocked in your browser';
        button.disabled = true;
        return;
      }

      const res = await fetch('/push/key', { credentials: 'same-origin' });
      const { key } = await res.json();
      if (!key) throw new Error('no key');

      const sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlBase64ToUint8Array(key),
      });

      const saved = await fetch('/push/subscribe', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(sub),
      });
      if (!saved.ok) throw new Error('save failed');

      button.textContent = "You'll get your brief at 6am";
      button.disabled = true;
    } catch (e) {
      // The usual cause is a browser that cannot reach its push service, which
      // is nothing the person reading this can fix. Keep the message plain and
      // put the real reason where it can be debugged.
      console.warn('push subscription failed:', e);
      button.textContent = "Couldn't turn notifications on";
      button.disabled = true;
    }
  }

  // The VAPID key arrives base64url-encoded; the subscribe API wants bytes.
  function urlBase64ToUint8Array(base64) {
    const padded = (base64 + '='.repeat((4 - (base64.length % 4)) % 4))
      .replace(/-/g, '+')
      .replace(/_/g, '/');
    const raw = atob(padded);
    return Uint8Array.from([...raw].map((c) => c.charCodeAt(0)));
  }

  document.addEventListener('click', function (event) {
    const button = event.target.closest ? event.target.closest('#enable-push') : null;
    if (button) enableNotifications(button);
  });

  // --- live search --------------------------------------------------------
  // An input marked data-search="<url>" fetches into data-target as the user
  // types. This is the whole reason htmx was here; fifteen lines is a better
  // trade than a CDN dependency for one box.
  function wireSearch(input) {
    var url = input.getAttribute('data-search');
    var target = document.querySelector(input.getAttribute('data-target'));
    if (!url || !target) return;

    var timer = null;
    var latest = 0;

    input.addEventListener('input', function () {
      clearTimeout(timer);
      timer = setTimeout(function () {
        var seq = ++latest;
        fetch(url + '?q=' + encodeURIComponent(input.value), {
          headers: { 'Accept': 'text/html' },
          credentials: 'same-origin',
        })
          .then(function (res) { return res.ok ? res.text() : ''; })
          .then(function (html) {
            // Out-of-order replies must not overwrite a newer result.
            if (seq === latest) target.innerHTML = html;
          })
          .catch(function () {
            if (seq === latest) target.textContent = "Couldn't search just now.";
          });
      }, 250);
    });
  }

  document.addEventListener('DOMContentLoaded', function () {
    document.querySelectorAll('[data-search]').forEach(wireSearch);
  });
})();
