package server

// dashboardHTML is the embedded single-page dashboard, restyled to match the
// Bifrost web UI design language: Geist font, the zinc/shadcn palette, the
// signature teal accent (oklch 0.5081 0.1049 165.61), a left sidebar
// workspace layout, and light/dark themes. Vanilla JS, no build step — it
// polls /status every 2s.
const dashboardHTML = `<!doctype html>
<html lang="en" class="dark">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Frostgate · Dashboard</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Geist+Mono:wght@300..700&family=Geist:wght@100..900&display=swap" rel="stylesheet">
<style>
  /* ─── Bifrost-style design tokens (zinc base + teal primary) ─── */
  :root {
    --radius: 0.5rem;
    --background: #f4f4f5;
    --foreground: oklch(0.141 0.005 285.823);
    --card: oklch(1 0 0);
    --card-foreground: oklch(0.141 0.005 285.823);
    --primary: oklch(0.5081 0.1049 165.61);
    --primary-foreground: oklch(0.985 0 0);
    --secondary: oklch(0.967 0.001 286.375);
    --muted: oklch(0.967 0.001 286.375);
    --muted-foreground: oklch(0.552 0.016 285.938);
    --accent: oklch(0.967 0.001 286.375);
    --accent-foreground: oklch(0.21 0.006 285.885);
    --destructive: oklch(0.577 0.245 27.325);
    --border: oklch(0.92 0.004 286.32);
    --ring: oklch(0.705 0.015 286.067);
    --sidebar: oklch(0.985 0 0);
    --brand: oklch(0.5081 0.1049 165.61);
    --brand-soft: oklch(0.5081 0.1049 165.61 / 0.12);
  }
  html.dark {
    --background: oklch(0.141 0.005 285.823);
    --foreground: oklch(0.985 0 0);
    --card: oklch(0.21 0.006 285.885);
    --card-foreground: oklch(0.985 0 0);
    --primary: oklch(0.92 0.004 286.32);
    --primary-foreground: oklch(0.21 0.006 285.885);
    --secondary: oklch(0.274 0.006 286.033);
    --muted: oklch(0.274 0.006 286.033);
    --muted-foreground: oklch(0.705 0.015 286.067);
    --accent: oklch(0.274 0.006 286.033);
    --accent-foreground: oklch(0.985 0 0);
    --destructive: oklch(0.704 0.191 22.216);
    --border: oklch(1 0 0 / 0.10);
    --ring: oklch(0.552 0.016 285.938);
    --sidebar: oklch(0.18 0.006 285.885);
    --brand: oklch(0.74 0.13 165.61);
    --brand-soft: oklch(0.74 0.13 165.61 / 0.14);
  }

  * { box-sizing: border-box; }
  html, body { height: 100%; }
  body {
    margin: 0;
    font-family: "Geist", ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    font-size: 0.95rem;
    background: var(--background);
    color: var(--foreground);
    -webkit-font-smoothing: antialiased;
  }
  code, .mono { font-family: "Geist Mono", ui-monospace, SFMono-Regular, Menlo, monospace; }

  .app { display: grid; grid-template-columns: 248px 1fr; min-height: 100vh; }

  /* ─── Sidebar ─── */
  .sidebar {
    background: var(--sidebar);
    border-right: 1px solid var(--border);
    display: flex; flex-direction: column;
    position: sticky; top: 0; height: 100vh;
  }
  .brand { display: flex; align-items: center; gap: 10px; padding: 18px 18px 14px; }
  .brand .mark {
    width: 30px; height: 30px; border-radius: 8px; flex: none;
    background: linear-gradient(140deg, var(--brand), oklch(0.62 0.13 195));
    display: grid; place-items: center; color: #fff;
    box-shadow: 0 2px 8px var(--brand-soft);
  }
  .brand .name { font-weight: 650; font-size: 0.98rem; letter-spacing: -0.01em; }
  .brand .sub { font-size: 0.7rem; color: var(--muted-foreground); margin-top: -2px; }
  .nav { padding: 6px 10px; display: flex; flex-direction: column; gap: 2px; overflow-y: auto; }
  .nav .label { font-size: 0.68rem; text-transform: uppercase; letter-spacing: 0.07em;
                color: var(--muted-foreground); padding: 12px 10px 4px; font-weight: 600; }
  .nav a {
    display: flex; align-items: center; gap: 10px; padding: 7px 10px; border-radius: var(--radius);
    color: var(--muted-foreground); text-decoration: none; font-size: 0.85rem; font-weight: 500;
    transition: background .12s, color .12s;
  }
  .nav a:hover { background: var(--accent); color: var(--accent-foreground); }
  .nav a.active { background: var(--brand-soft); color: var(--brand); }
  .nav a.active svg { color: var(--brand); }
  .nav svg { width: 16px; height: 16px; flex: none; }
  .sidebar .foot { margin-top: auto; padding: 12px 16px; border-top: 1px solid var(--border);
                   font-size: 0.72rem; color: var(--muted-foreground); }

  /* ─── Header ─── */
  .main { display: flex; flex-direction: column; min-width: 0; }
  header {
    position: sticky; top: 0; z-index: 5;
    display: flex; align-items: center; gap: 14px;
    padding: 0 24px; height: 56px;
    background: color-mix(in oklch, var(--background) 80%, transparent);
    backdrop-filter: blur(8px);
    border-bottom: 1px solid var(--border);
  }
  header h1 { font-size: 1rem; font-weight: 600; margin: 0; letter-spacing: -0.01em; }
  header .crumb { color: var(--muted-foreground); font-size: 0.85rem; }
  header .spacer { flex: 1; }
  .live { display: inline-flex; align-items: center; gap: 7px; font-size: 0.78rem;
          color: var(--muted-foreground); padding: 4px 10px; border: 1px solid var(--border);
          border-radius: 999px; }
  .live .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--brand);
               box-shadow: 0 0 0 0 var(--brand); animation: pulse 2s infinite; }
  @keyframes pulse { 0% { box-shadow: 0 0 0 0 var(--brand-soft); } 70% { box-shadow: 0 0 0 6px transparent; } 100% { box-shadow: 0 0 0 0 transparent; } }
  .iconbtn { display: grid; place-items: center; width: 34px; height: 34px; border-radius: var(--radius);
             border: 1px solid var(--border); background: var(--card); color: var(--foreground);
             cursor: pointer; }
  .iconbtn:hover { background: var(--accent); }
  .iconbtn svg { width: 16px; height: 16px; }

  /* ─── Content ─── */
  .content { padding: 24px; max-width: 1180px; width: 100%; }
  section { scroll-margin-top: 72px; }
  h2 { font-size: 0.72rem; text-transform: uppercase; letter-spacing: 0.07em;
       color: var(--muted-foreground); font-weight: 600; margin: 30px 0 12px; }
  h2:first-of-type { margin-top: 6px; }

  .grid { display: grid; gap: 14px; }
  .stats { grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); }
  .cfg { grid-template-columns: repeat(auto-fit, minmax(190px, 1fr)); }

  .card { background: var(--card); border: 1px solid var(--border); border-radius: var(--radius); padding: 16px 18px; }
  .stat .l { font-size: 0.75rem; color: var(--muted-foreground); font-weight: 500; }
  .stat .v { font-size: 1.9rem; font-weight: 680; letter-spacing: -0.02em; margin-top: 6px; line-height: 1; }
  .stat.accent .v { color: var(--brand); }

  .kv { display: flex; align-items: center; justify-content: space-between; }
  .kv .l { font-size: 0.78rem; color: var(--muted-foreground); }

  table { width: 100%; border-collapse: collapse; }
  th, td { text-align: left; padding: 10px 12px; border-bottom: 1px solid var(--border); font-size: 0.85rem; }
  th { color: var(--muted-foreground); font-weight: 500; font-size: 0.74rem; text-transform: uppercase; letter-spacing: 0.04em; }
  tr:last-child td { border-bottom: none; }
  td.mono, code { font-size: 0.8rem; }
  code { background: var(--secondary); border: 1px solid var(--border); border-radius: 6px; padding: 1px 6px; color: var(--foreground); }

  .badge { display: inline-flex; align-items: center; gap: 6px; font-size: 0.72rem; font-weight: 500;
           padding: 2px 9px; border-radius: 999px; border: 1px solid var(--border); }
  .badge.on { color: var(--brand); border-color: var(--brand-soft); background: var(--brand-soft); }
  .badge.off { color: var(--muted-foreground); background: var(--secondary); }
  .badge.danger { color: var(--destructive); border-color: color-mix(in oklch, var(--destructive) 30%, transparent); }

  .chain { display: inline-flex; align-items: center; gap: 6px; flex-wrap: wrap; }
  .arrow { color: var(--muted-foreground); }

  .bar { height: 6px; background: var(--secondary); border-radius: 999px; overflow: hidden; margin-top: 6px; }
  .bar > span { display: block; height: 100%; background: var(--brand); border-radius: 999px; }
  .muted { color: var(--muted-foreground); }
  .tool { display: inline-flex; align-items: center; gap: 6px; padding: 4px 10px; border: 1px solid var(--border);
          border-radius: var(--radius); font-size: 0.8rem; margin: 0 6px 6px 0; }
  .tool .dot { width: 6px; height: 6px; border-radius: 50%; background: var(--brand); }
  .empty { color: var(--muted-foreground); font-size: 0.85rem; padding: 4px 2px; }

  @media (max-width: 820px) { .app { grid-template-columns: 1fr; } .sidebar { display: none; } }
</style>
</head>
<body>
<div class="app">
  <!-- Sidebar -->
  <aside class="sidebar">
    <div class="brand">
      <div class="mark">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M3 16c4-8 14-8 18 0"/><path d="M3 16h18"/></svg>
      </div>
      <div>
        <div class="name">Frostgate</div>
        <div class="sub">AI Gateway</div>
      </div>
    </div>
    <nav class="nav" id="nav">
      <div class="label">Workspace</div>
      <a href="#overview" class="active" data-s="overview">__I_dash__ Dashboard</a>
      <a href="#providers" data-s="providers">__I_srv__ Providers</a>
      <a href="#models" data-s="models">__I_box__ Models</a>
      <a href="#mcp" data-s="mcp">__I_tool__ MCP Tools</a>
      <div class="label">Platform</div>
      <a href="#governance" data-s="governance">__I_shield__ Governance</a>
      <a href="#cluster" data-s="cluster">__I_net__ Cluster</a>
      <a href="/metrics" target="_blank">__I_chart__ Metrics</a>
    </nav>
    <div class="foot">
      <div class="kv"><span>node</span><code id="footNode" class="mono">—</code></div>
    </div>
  </aside>

  <!-- Main -->
  <div class="main">
    <header>
      <h1>Dashboard</h1>
      <span class="crumb">/ overview</span>
      <span class="spacer"></span>
      <span class="live"><span class="dot"></span><span id="liveText">live</span></span>
      <button class="iconbtn" id="themeBtn" title="Toggle theme" onclick="toggleTheme()"></button>
    </header>

    <div class="content">
      <!-- Overview -->
      <section id="overview">
        <h2>Overview</h2>
        <div class="grid stats" id="stats"></div>
      </section>

      <h2>Runtime</h2>
      <div class="grid cfg" id="cfg"></div>

      <!-- Providers -->
      <section id="providers">
        <h2>Providers</h2>
        <div class="card" style="padding:0"><table id="providers"><thead>
          <tr><th>Name</th><th>Kind</th><th>API keys</th></tr></thead><tbody></tbody></table></div>
      </section>

      <!-- Models -->
      <section id="models">
        <h2>Models &amp; fallback chains</h2>
        <div class="card" style="padding:0"><table id="models"><thead>
          <tr><th>Alias</th><th>Routing chain</th></tr></thead><tbody></tbody></table></div>
      </section>

      <!-- MCP -->
      <section id="mcp">
        <h2>MCP Tools</h2>
        <div class="card" id="mcpCard"></div>
      </section>

      <!-- Governance -->
      <section id="governance">
        <h2>Virtual key usage</h2>
        <div class="card" style="padding:0"><table id="keys"><thead>
          <tr><th>Identity</th><th>Requests</th><th>RPM</th><th>Token budget</th></tr></thead><tbody></tbody></table>
          <div id="noKeys" class="empty" style="display:none;padding:14px 16px"></div>
        </div>
      </section>

      <!-- Cluster -->
      <section id="cluster">
        <h2>Cluster</h2>
        <div class="card" id="clusterCard"></div>
      </section>
    </div>
  </div>
</div>

<script>
// ─── Lucide-style icons (injected into nav labels) ───
var ICONS = {
  __I_dash__: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="7" height="9" rx="1"/><rect x="14" y="3" width="7" height="5" rx="1"/><rect x="14" y="12" width="7" height="9" rx="1"/><rect x="3" y="16" width="7" height="5" rx="1"/></svg>',
  __I_srv__: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="2" width="20" height="8" rx="2"/><rect x="2" y="14" width="20" height="8" rx="2"/><line x1="6" y1="6" x2="6.01" y2="6"/><line x1="6" y1="18" x2="6.01" y2="18"/></svg>',
  __I_box__: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/><polyline points="3.27 6.96 12 12.01 20.73 6.96"/><line x1="12" y1="22.08" x2="12" y2="12"/></svg>',
  __I_tool__: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/></svg>',
  __I_shield__: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg>',
  __I_net__: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="2" width="6" height="6" rx="1"/><rect x="2" y="16" width="6" height="6" rx="1"/><rect x="16" y="16" width="6" height="6" rx="1"/><path d="M12 8v4M5 16v-2h14v2"/></svg>',
  __I_chart__: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="20" x2="12" y2="10"/><line x1="18" y1="20" x2="18" y2="4"/><line x1="6" y1="20" x2="6" y2="16"/></svg>'
};
var SUN = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>';
var MOON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>';

// inject nav icons
document.querySelectorAll('#nav a').forEach(function(a){
  a.innerHTML = a.innerHTML.replace(/__I_\w+__/g, function(m){ return ICONS[m] || ''; });
});

function toggleTheme(){
  document.documentElement.classList.toggle('dark');
  document.getElementById('themeBtn').innerHTML = document.documentElement.classList.contains('dark') ? SUN : MOON;
}
document.getElementById('themeBtn').innerHTML = document.documentElement.classList.contains('dark') ? SUN : MOON;

function esc(s){ return String(s).replace(/[&<>]/g, function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;'}[c]; }); }
function badge(on, label){ return '<span class="badge '+(on?'on':'off')+'">'+esc(label)+'</span>'; }

// scroll-spy: highlight nav on scroll
var navLinks = Array.prototype.slice.call(document.querySelectorAll('#nav a[data-s]'));
window.addEventListener('scroll', function(){
  var y = window.scrollY + 90; var cur = navLinks[0];
  navLinks.forEach(function(a){
    var el = document.getElementById(a.dataset.s);
    if (el && el.offsetTop <= y) cur = a;
  });
  navLinks.forEach(function(a){ a.classList.toggle('active', a === cur); });
});

async function refresh(){
  var d;
  try { d = await (await fetch('/status')).json(); }
  catch (e) { document.getElementById('liveText').textContent = 'offline'; return; }
  document.getElementById('liveText').textContent = 'live · ' + esc(d.node || '');
  document.getElementById('footNode').textContent = d.node || '—';

  var c = d.counters;
  document.getElementById('stats').innerHTML = [
    ['Requests', c.requests, true], ['Cache hits', c.cache_hits, false],
    ['Errors', c.errors, false], ['Tokens saved', c.tokens_saved, true]
  ].map(function(x){
    return '<div class="card stat '+(x[2]?'accent':'')+'"><div class="l">'+x[0]+'</div><div class="v">'+x[1]+'</div></div>';
  }).join('');

  document.getElementById('cfg').innerHTML = [
    ['Cache', d.cache.enabled, d.cache.enabled?'enabled':'disabled'],
    ['Compression', d.compression.enabled, (d.compression.enabled?'on · ':'off · ')+d.compression.strategy],
    ['Governance', d.governance.enabled, d.governance.enabled?'enforced':'open'],
    ['MCP', d.mcp.enabled, d.mcp.enabled?(d.mcp.tools.length+' tools'):'off'],
    ['Persistence', d.persistence.enabled, d.persistence.enabled?'durable':'memory'],
    ['Cluster', d.cluster.enabled, d.cluster.enabled?('node '+d.cluster.node_id):'single']
  ].map(function(x){
    return '<div class="card"><div class="kv"><span class="l">'+x[0]+'</span>'+badge(x[1], x[2])+'</div></div>';
  }).join('');

  document.querySelector('#providers tbody').innerHTML = d.providers.map(function(p){
    return '<tr><td>'+esc(p.name)+'</td><td><code>'+esc(p.kind)+'</code></td><td class="muted">'+p.keys+'</td></tr>';
  }).join('');

  document.querySelector('#models tbody').innerHTML = d.models.map(function(m){
    var chain = m.chain.map(function(x){ return '<code>'+esc(x)+'</code>'; }).join('<span class="arrow">→</span>');
    return '<tr><td><code>'+esc(m.alias)+'</code></td><td><span class="chain">'+chain+'</span></td></tr>';
  }).join('');

  var tools = (d.mcp && d.mcp.tools) || [];
  document.getElementById('mcpCard').innerHTML = tools.length
    ? tools.map(function(t){ return '<span class="tool"><span class="dot"></span>'+esc(t)+'</span>'; }).join('')
    : '<div class="empty">No MCP servers connected. Enable <code>mcp</code> in config to expose tools.</div>';

  var keys = (d.governance && d.governance.keys) || [];
  var showKeys = keys.length > 0;
  document.querySelector('#keys').style.display = showKeys ? '' : 'none';
  var noKeys = document.getElementById('noKeys');
  noKeys.style.display = showKeys ? 'none' : 'block';
  noKeys.textContent = d.governance.enabled ? 'No identities yet.' : 'Governance is disabled — the gateway is open (no key required).';
  document.querySelector('#keys tbody').innerHTML = keys.map(function(k){
    var budget = '<span class="muted">unlimited</span>';
    if (k.max_tokens > 0) {
      var pct = Math.min(100, 100 * k.spent_tokens / k.max_tokens);
      budget = '<div>'+k.spent_tokens.toLocaleString()+' / '+k.max_tokens.toLocaleString()+'</div><div class="bar"><span style="width:'+pct+'%"></span></div>';
    } else if (k.spent_tokens > 0) {
      budget = k.spent_tokens.toLocaleString()+' <span class="muted">used</span>';
    }
    return '<tr><td>'+esc(k.name)+'</td><td>'+k.requests+'</td><td class="muted">'+(k.rpm||'∞')+'</td><td>'+budget+'</td></tr>';
  }).join('');

  document.getElementById('clusterCard').innerHTML =
    '<div class="kv"><span class="l">Node ID</span><code>'+esc(d.cluster.node_id||d.node||'—')+'</code></div>'+
    '<div class="kv" style="margin-top:10px"><span class="l">Mode</span>'+badge(d.cluster.enabled,'cluster '+(d.cluster.enabled?'on':'off'))+'</div>'+
    '<div class="kv" style="margin-top:10px"><span class="l">Shared state</span><span class="muted mono">'+esc(d.persistence.enabled?d.persistence.path:'memory (node-local)')+'</span></div>';
}
refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>`
