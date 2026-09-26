// Notificaciones tipo "toast" que reemplazan a alert(): no bloquean la
// pantalla ni piden un click extra.
//
//   toast('¡Comanda registrada!')           // éxito, se oculta sola
//   toast('Selecciona una mesa', 'warn')
//   toast('No se pudo imprimir', 'error')   // se queda más tiempo
//   toast.next('Mensaje', 'warn')           // se muestra al cargar la siguiente página
(function () {
  var STYLES = {
    ok: { icon: '✅', bg: '#065f46', border: '#10b981', ms: 3500 },
    warn: { icon: '⚠️', bg: '#78350f', border: '#f59e0b', ms: 6000 },
    error: { icon: '⛔', bg: '#881337', border: '#f43f5e', ms: 10000 },
  };
  var MAX_VISIBLE = 4;
  var NEXT_KEY = 'open-pos:toast-next';

  var css = document.createElement('style');
  css.textContent =
    '#toasts{position:fixed;top:76px;right:16px;z-index:9999;display:flex;flex-direction:column;gap:8px;width:min(360px,calc(100vw - 32px));pointer-events:none}' +
    '.toast{pointer-events:auto;display:flex;gap:10px;align-items:flex-start;color:#fff;padding:12px 14px;border-radius:14px;border-left:4px solid;' +
    'box-shadow:0 10px 25px rgba(0,0,0,.25);font:600 13px/1.4 ui-sans-serif,system-ui,sans-serif;white-space:pre-line;cursor:pointer;' +
    'animation:toast-in .18s ease-out}' +
    '.toast.out{opacity:0;transform:translateX(24px);transition:opacity .2s,transform .2s}' +
    '.toast .x{margin-left:auto;opacity:.6;font-size:16px;line-height:1}' +
    '@keyframes toast-in{from{opacity:0;transform:translateX(24px)}to{opacity:1;transform:none}}';
  document.head.appendChild(css);

  function container() {
    var el = document.getElementById('toasts');
    if (!el) {
      el = document.createElement('div');
      el.id = 'toasts';
      el.setAttribute('role', 'status');
      el.setAttribute('aria-live', 'polite');
      document.body.appendChild(el);
    }
    return el;
  }

  function dismiss(el) {
    if (el.classList.contains('out')) return;
    el.classList.add('out');
    setTimeout(function () { el.remove(); }, 200);
  }

  function toast(text, type) {
    var s = STYLES[type] || STYLES.ok;
    var box = container();
    while (box.children.length >= MAX_VISIBLE) box.firstChild.remove();

    var el = document.createElement('div');
    el.className = 'toast';
    el.style.background = s.bg;
    el.style.borderColor = s.border;
    el.title = 'Click para cerrar';

    var icon = document.createElement('span');
    icon.textContent = s.icon;
    var msg = document.createElement('span');
    msg.textContent = text; // textContent: nunca interpretar el mensaje como HTML
    var close = document.createElement('span');
    close.className = 'x';
    close.textContent = '×';
    el.append(icon, msg, close);

    el.addEventListener('click', function () { dismiss(el); });
    box.appendChild(el);
    setTimeout(function () { dismiss(el); }, s.ms);
  }

  toast.next = function (text, type) {
    try { sessionStorage.setItem(NEXT_KEY, JSON.stringify({ text: text, type: type })); } catch (e) {}
  };

  function showPending() {
    try {
      var raw = sessionStorage.getItem(NEXT_KEY);
      if (!raw) return;
      sessionStorage.removeItem(NEXT_KEY);
      var t = JSON.parse(raw);
      toast(t.text, t.type);
    } catch (e) {}
  }

  window.toast = toast;
  if (document.body) showPending();
  else document.addEventListener('DOMContentLoaded', showPending);
})();
