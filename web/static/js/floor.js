// Plano del salón: utilidades compartidas por el editor (admin) y el POS.
// Posiciones y tamaños van en celdas de la cuadrícula; aquí se pasan a px.
(function () {
  var MAX_COLS = 60, MAX_ROWS = 40, MAX_SIZE = 12;

  var KINDS = {
    WALL: { name: 'Pared', cls: 'bg-slate-600 rounded-sm' },
    COUNTER: { name: 'Barra', cls: 'bg-amber-100 border-2 border-amber-300 text-amber-900 rounded-lg' },
    DOOR: { name: 'Puerta', cls: 'bg-sky-50 border-2 border-dashed border-sky-300 text-sky-700 rounded-lg' },
    LABEL: { name: 'Texto', cls: 'text-gray-400 uppercase tracking-wider' },
  };

  function overlaps(a, b) {
    return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
  }

  function inBounds(r) {
    return r.x >= 0 && r.y >= 0 && r.w >= 1 && r.h >= 1 && r.w <= MAX_SIZE && r.h <= MAX_SIZE &&
      r.x + r.w <= MAX_COLS && r.y + r.h <= MAX_ROWS;
  }

  // Rectángulo que abarca todo lo dibujado, para recortar el plano en el POS.
  function bounds(els) {
    if (!els.length) return { x: 0, y: 0, cols: 1, rows: 1 };
    var x0 = Infinity, y0 = Infinity, x1 = 0, y1 = 0;
    els.forEach(function (e) {
      x0 = Math.min(x0, e.x); y0 = Math.min(y0, e.y);
      x1 = Math.max(x1, e.x + e.w); y1 = Math.max(y1, e.y + e.h);
    });
    return { x: x0, y: y0, cols: x1 - x0, rows: y1 - y0 };
  }

  // Tamaño de celda para que cols quepan en width, entre min y max px.
  function cellFor(width, cols, min, max) {
    return Math.max(min, Math.min(max, Math.floor(width / cols)));
  }

  // Estilo en px de un elemento; inset deja un pequeño pasillo visual.
  function box(r, cell, origin, inset) {
    inset = inset || 0;
    origin = origin || { x: 0, y: 0 };
    return 'left:' + ((r.x - origin.x) * cell + inset) + 'px;top:' + ((r.y - origin.y) * cell + inset) +
      'px;width:' + (r.w * cell - 2 * inset) + 'px;height:' + (r.h * cell - 2 * inset) + 'px';
  }

  // Tamaño de letra que cabe en el elemento.
  function fontFor(r, cell, max) {
    return Math.max(9, Math.min(max || 18, Math.floor(Math.min(r.w, r.h) * cell / 3.2)));
  }

  function minutes(m) {
    if (m < 60) return m + ' min';
    return Math.floor(m / 60) + ' h ' + (m % 60 < 10 ? '0' : '') + (m % 60);
  }

  window.Floor = {
    MAX_COLS: MAX_COLS, MAX_ROWS: MAX_ROWS, MAX_SIZE: MAX_SIZE, KINDS: KINDS,
    overlaps: overlaps, inBounds: inBounds, bounds: bounds, cellFor: cellFor,
    box: box, fontFor: fontFor, minutes: minutes,
  };
})();
