// Gráficas de columnas en SVG, sin dependencias: el POS funciona sin internet.
//
//   columnChart(el, {
//     data: [{ label: 'Lun', values: [120, 40], tip: ['Lun', '$160'] }],  // 1a línea en negritas
//     series: [{ name: 'Efectivo', color: '#2a78d6' }, ...],
//     format: fn,       // formato de los ticks del eje Y
//     labelEvery: 1,    // cada cuántas columnas se rotula el eje X
//   })
//
// Una sola serie = columnas simples; varias = apiladas con 2px de separación.
// Columnas de máximo 24px con extremo redondeado de 4px, cuadrícula de 1px y
// tooltip por columna con zona de toque más grande que la barra.
(function () {
  var NS = 'http://www.w3.org/2000/svg';
  var GRID = '#e5e4e0';
  var AXIS_TEXT = '#52514e';
  var SURFACE = '#ffffff';

  var tip = null;
  function tipEl() {
    if (!tip) {
      tip = document.createElement('div');
      tip.style.cssText = 'position:fixed;z-index:60;pointer-events:none;background:#0b0b0b;color:#fff;' +
        'font:12px/1.4 ui-sans-serif,system-ui,sans-serif;padding:6px 9px;border-radius:8px;' +
        'box-shadow:0 6px 16px rgba(0,0,0,.2);display:none;white-space:nowrap';
      document.body.appendChild(tip);
    }
    return tip;
  }
  // lines: la primera va en negritas. Se arma con textContent, nunca como HTML.
  function showTip(evt, lines) {
    var t = tipEl();
    t.replaceChildren();
    lines.forEach(function (line, i) {
      var el = document.createElement(i === 0 ? 'b' : 'div');
      el.style.display = 'block';
      el.textContent = line;
      t.appendChild(el);
    });
    t.style.display = 'block';
    var x = evt.clientX + 14, y = evt.clientY - t.offsetHeight - 10;
    if (x + t.offsetWidth > window.innerWidth - 8) x = evt.clientX - t.offsetWidth - 14;
    if (y < 8) y = evt.clientY + 16;
    t.style.left = x + 'px';
    t.style.top = y + 'px';
  }
  function hideTip() { if (tip) tip.style.display = 'none'; }

  function niceStep(max, ticks) {
    if (max <= 0) return 1;
    var raw = max / ticks;
    var mag = Math.pow(10, Math.floor(Math.log10(raw)));
    var n = raw / mag;
    return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10) * mag;
  }

  function node(name, attrs, parent) {
    var n = document.createElementNS(NS, name);
    for (var k in attrs) n.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(n);
    return n;
  }

  // Columna con la punta redondeada y la base cuadrada.
  function barPath(x, y, w, h, round) {
    var r = round ? Math.min(4, h, w / 2) : 0;
    return 'M' + x + ',' + (y + h) + 'V' + (y + r) +
      (r ? 'Q' + x + ',' + y + ' ' + (x + r) + ',' + y : '') +
      'H' + (x + w - r) +
      (r ? 'Q' + (x + w) + ',' + y + ' ' + (x + w) + ',' + (y + r) : '') +
      'V' + (y + h) + 'Z';
  }

  function render(el, opts) {
    el.innerHTML = '';
    var data = opts.data || [];
    var series = opts.series || [{ color: '#2a78d6' }];
    var fmt = opts.format || function (v) { return String(v); };
    var W = Math.max(el.clientWidth, 200), H = opts.height || 220;
    var m = { top: 10, right: 6, bottom: 24, left: 52 };
    var pw = W - m.left - m.right, ph = H - m.top - m.bottom;

    var totals = data.map(function (d) { return d.values.reduce(function (a, b) { return a + b; }, 0); });
    var max = Math.max.apply(null, totals.concat([0]));
    var step = niceStep(max, 4), top = Math.max(step, Math.ceil(max / step) * step);
    var y = function (v) { return m.top + ph - (v / top) * ph; };

    var svg = node('svg', { width: W, height: H, viewBox: '0 0 ' + W + ' ' + H, role: 'img', style: 'display:block' }, el);
    if (opts.title) node('title', {}, svg).textContent = opts.title;

    for (var v = 0; v <= top + 1e-9; v += step) {
      node('line', { x1: m.left, x2: W - m.right, y1: y(v), y2: y(v), stroke: GRID, 'stroke-width': 1, 'shape-rendering': 'crispEdges' }, svg);
      var t = node('text', { x: m.left - 8, y: y(v) + 4, 'text-anchor': 'end', 'font-size': 11, fill: AXIS_TEXT }, svg);
      t.textContent = fmt(v);
    }

    var band = pw / Math.max(data.length, 1);
    var bw = Math.max(2, Math.min(24, band * 0.7));
    var every = opts.labelEvery || Math.max(1, Math.ceil(data.length / Math.floor(pw / 44)));

    data.forEach(function (d, i) {
      var x = m.left + band * i + (band - bw) / 2;
      var base = m.top + ph;
      var visible = d.values.map(function (v, s) { return { v: v, s: s }; }).filter(function (p) { return p.v > 0; });
      visible.forEach(function (p, k) {
        var h = (p.v / top) * ph;
        var gap = k < visible.length - 1 ? 2 : 0; // separación en el color de la superficie
        var segTop = base - h;
        if (h - gap > 0.5) {
          node('path', { d: barPath(x, segTop, bw, h - gap, k === visible.length - 1), fill: series[p.s].color }, svg);
        }
        base = segTop;
      });
      if (i % every === 0) {
        var lbl = node('text', { x: x + bw / 2, y: H - 6, 'text-anchor': 'middle', 'font-size': 11, fill: AXIS_TEXT }, svg);
        lbl.textContent = d.label;
      }
      // Zona de toque: toda la franja, no solo la barra.
      var hit = node('rect', { x: m.left + band * i, y: m.top, width: band, height: ph, fill: SURFACE, 'fill-opacity': 0 }, svg);
      hit.style.cursor = 'default';
      hit.addEventListener('mousemove', function (e) { showTip(e, d.tip); hit.setAttribute('fill-opacity', 0.04); hit.setAttribute('fill', '#0b0b0b'); });
      hit.addEventListener('mouseleave', function () { hideTip(); hit.setAttribute('fill-opacity', 0); });
    });

    node('line', { x1: m.left, x2: W - m.right, y1: m.top + ph, y2: m.top + ph, stroke: '#c9c8c3', 'stroke-width': 1, 'shape-rendering': 'crispEdges' }, svg);
  }

  window.columnChart = function (el, opts) {
    // Sin esto el SVG de ancho fijo no deja que el contenedor se encoja y la
    // gráfica nunca se vuelve a dibujar al hacer la ventana más angosta.
    el.style.overflow = 'hidden';
    el.style.minWidth = '0';
    render(el, opts);
    if (!el._vizObserver && window.ResizeObserver) {
      var last = el.clientWidth;
      el._vizObserver = new ResizeObserver(function () {
        if (Math.abs(el.clientWidth - last) > 4) { last = el.clientWidth; render(el, el._vizOpts); }
      });
      el._vizObserver.observe(el);
    }
    el._vizOpts = opts;
  };
})();
