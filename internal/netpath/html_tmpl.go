package netpath

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>lmon path</title>
<style>
:root{--bg:#fff;--fg:#1b1f23;--muted:#57606a;--ocean:#e3edf7;--land:#c9d3dc;--border:#fff;--halo:#ffffffd9;--card:#f6f8fa;--line:#d0d7de;--accent:#0969da}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--fg:#e6edf3;--muted:#8b949e;--ocean:#0e1a29;--land:#2b3745;--border:#0d1117;--halo:#0d1117d9;--card:#161b22;--line:#30363d;--accent:#58a6ff}}
*{box-sizing:border-box}
body{margin:0 auto;max-width:1100px;padding:16px;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
h1{font-size:1.4rem;margin:0 0 2px}
.sub,.meta,footer{color:var(--muted);font-size:.85rem}
.controls{display:flex;flex-wrap:wrap;gap:12px 24px;align-items:flex-start;margin:14px 0}
.views button{font:inherit;padding:4px 12px;border:1px solid var(--line);background:var(--card);color:var(--fg);cursor:pointer}
.views button:first-child{border-radius:6px 0 0 6px}.views button:last-child{border-radius:0 6px 6px 0;margin-left:-1px}
.views button[aria-pressed=true]{background:var(--accent);border-color:var(--accent);color:#fff}
.routes{list-style:none;margin:0;padding:0;display:flex;flex-direction:column;gap:2px}
.routes label{cursor:pointer}
.sw{display:inline-block;width:12px;height:12px;border-radius:3px;vertical-align:-1px;margin:0 4px}
.mapwrap{border:1px solid var(--line);border-radius:8px;overflow:hidden}
svg#map{display:block;width:100%;height:auto}
.ocean{fill:var(--ocean)}
.land{fill:var(--land);stroke:var(--border);stroke-width:.6;vector-effect:non-scaling-stroke}
.arc{fill:none;stroke:currentColor;stroke-width:2;vector-effect:non-scaling-stroke;opacity:.85}
.arc.ghost{stroke:var(--muted);stroke-width:1.5;stroke-dasharray:5 4;opacity:.8}
.sc{transform:scale(calc(1 / var(--z)))}
.dot{fill:currentColor;stroke:var(--bg);stroke-width:1.5;vector-effect:non-scaling-stroke}
.you .dot{fill:var(--fg)}
.excluded{fill:none;stroke:var(--muted);stroke-width:1.2;opacity:.7;vector-effect:non-scaling-stroke}
.registered{fill:var(--bg);stroke:var(--muted);stroke-width:1.6;stroke-dasharray:2.5 2;vector-effect:non-scaling-stroke}
.diamond{fill:currentColor;stroke:var(--bg);stroke-width:1.5;vector-effect:non-scaling-stroke}
.lbl{font-size:10px;fill:var(--fg);paint-order:stroke;stroke:var(--halo);stroke-width:3px;stroke-linejoin:round;vector-effect:non-scaling-stroke}
.lbl.reg{fill:var(--muted)}
.legend{display:flex;flex-wrap:wrap;gap:6px 18px;margin:10px 0;font-size:.85rem;color:var(--muted)}
.legend svg{vertical-align:-3px}
.note{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:10px 16px;margin:14px 0;font-size:.9rem}
.note ul{margin:6px 0 0;padding-left:20px}
details{border:1px solid var(--line);border-radius:8px;margin:10px 0;padding:6px 12px}
summary{cursor:pointer}
.tablewrap{overflow-x:auto}
table{border-collapse:collapse;width:100%;font-size:.82rem;margin:8px 0}
th,td{text-align:left;padding:3px 10px 3px 0;white-space:nowrap}
th{color:var(--muted);font-weight:600;border-bottom:1px solid var(--line)}
td.host,td.notes{white-space:normal;word-break:break-word}
tr.silent td,tr.private td{color:var(--muted)}
footer{margin-top:18px}
</style>
</head>
<body>
<header>
<h1>Where traffic to LLM endpoints goes</h1>
<p class="sub">Traced {{.Generated}} by <code>lmon path</code>{{if .Viewpoints}} · viewpoint: {{.Viewpoints}}{{end}}</p>
</header>

<div class="controls">
<div class="views" role="group" aria-label="Map view">
<button type="button" data-view="world" aria-pressed="true">World</button><button type="button" data-view="zoom" aria-pressed="false">Zoom to route</button>
</div>
<ul class="routes">
{{range .Paths}}<li><label><input type="checkbox" data-route="{{.ID}}" checked><span class="sw" style="background:{{.Color}}"></span><b>{{.Host}}</b></label> <span class="meta">{{.Route}}</span></li>
{{end}}</ul>
</div>

<div class="mapwrap">
<svg id="map" viewBox="{{.ViewWorld}}" data-world="{{.ViewWorld}}" data-zoom="{{.ViewZoom}}" data-zf="{{f1 .ZoomFactor}}" preserveAspectRatio="xMidYMid meet" role="img" aria-label="World map showing the observed route to each endpoint" style="--z:1">
<rect class="ocean" x="0" y="0" width="1000" height="500"/>
<path class="land" fill-rule="evenodd" d="{{.World}}"/>
{{range $p := .Paths}}<g id="{{$p.ID}}" color="{{$p.Color}}">
{{range $p.Arcs}}<path class="arc{{if .Dashed}} ghost{{end}}" d="{{.D}}"/>
{{end}}{{range $p.Markers}}<g transform="translate({{f1 .X}} {{f1 .Y}})"><g class="sc">
{{if eq .Kind "you"}}<g class="you"><circle class="dot" r="5"/></g>
{{else if eq .Kind "hop"}}<circle class="dot" r="3.5"/>
{{else if eq .Kind "edge"}}<path class="diamond" d="M0 -7 L7 0 L0 7 L-7 0 Z"/>
{{else if eq .Kind "dest"}}<path class="diamond" d="M0 -7 L7 0 L0 7 L-7 0 Z"/>
{{else if eq .Kind "registered"}}<circle class="registered" r="6"/>
{{else}}<circle class="excluded" r="3.5"/>
{{end}}{{if .Label}}<text class="lbl{{if eq .Kind "registered"}} reg{{end}}" x="9" y="{{f1 .DY}}" dy="3">{{.Label}}</text>
{{end}}<title>{{.Tip}}</title></g></g>
{{end}}</g>
{{end}}</svg>
</div>

<div class="legend" aria-hidden="true">
<span><svg width="14" height="14"><circle cx="7" cy="7" r="5" fill="currentColor"/></svg> you</span>
<span><svg width="14" height="14"><circle cx="7" cy="7" r="3.5" fill="#0072B2"/></svg> observed router</span>
<span><svg width="14" height="14"><path d="M7 1 L13 7 L7 13 L1 7 Z" fill="#0072B2"/></svg> edge: where traffic enters the provider</span>
<span><svg width="14" height="14"><circle cx="7" cy="7" r="4" fill="none" stroke="currentColor" stroke-dasharray="2.5 2"/></svg> where GeoIP says the address is registered</span>
<span><svg width="14" height="14"><circle cx="7" cy="7" r="3.5" fill="none" stroke="currentColor"/></svg> ruled out by latency, not connected</span>
</div>

<div class="note">
<b>How to read this, and what it cannot show</b>
<ul>
<li><b>Solid routes</b> are routers that answered traceroute probes. Each is placed from its hostname (city codes such as <code>par1</code>) and from GeoIP, then checked against its round-trip time: a router cannot be farther away than light in fibre allows.</li>
<li><b>The edge</b> is where your traffic enters the provider's network. A traceroute can go no further than that.</li>
<li><b>The dashed line</b> to a hollow ring is where GeoIP says the endpoint address is registered. When the round trip is too short for that place, the address is anycast or an edge server near you, and the ring is <b>not</b> where your data goes.</li>
<li><b>Beyond the edge</b>, inside the provider's own network and possibly in other countries, is <b>not observable from here</b>. Only the provider's documentation and regional endpoints can tell you that.</li>
<li>Locations of routers are estimates. Hostnames are usually right, GeoIP often is not, and your own location is estimated unless you gave it with <code>--from</code>.</li>
</ul>
</div>

{{range .Paths}}<details>
<summary><b>{{.Host}}</b> · {{.Dest}} · countries on the path: {{.Observed}}</summary>
<p>{{.Endpoint}}</p>
<div class="tablewrap"><table>
<thead><tr><th>Hop</th><th>Address</th><th>RTT</th><th>Place</th><th>Source</th><th>Confidence</th><th>Hostname</th><th>Notes</th></tr></thead>
<tbody>
{{range .Hops}}<tr class="{{.Kind}}"><td>{{.TTL}}</td>{{if eq .Kind "silent"}}<td colspan="7">* no reply</td>{{else if eq .Kind "private"}}<td>{{.Addr}}</td><td>{{.RTT}}</td><td colspan="5">local network</td>{{else}}<td>{{.Addr}}</td><td>{{.RTT}}</td><td>{{.Place}}</td><td>{{.Source}}</td><td>{{.Conf}}</td><td class="host">{{.Host}}</td><td class="notes">{{.Notes}}</td>{{end}}</tr>
{{end}}</tbody>
</table></div>
</details>
{{end}}
<footer>Map: Natural Earth (public domain).{{if .Attribution}} {{.Attribution}}.{{end}} Generated by lmon. Estimates only: see the notes above.</footer>

<script>
(function(){
var svg=document.getElementById('map');
var view={world:svg.getAttribute('data-world'),zoom:svg.getAttribute('data-zoom')};
var zf={world:1,zoom:parseFloat(svg.getAttribute('data-zf'))||1};
var buttons=document.querySelectorAll('[data-view]');
function setView(v){
  svg.setAttribute('viewBox',view[v]);
  svg.style.setProperty('--z',zf[v]);
  buttons.forEach(function(b){b.setAttribute('aria-pressed',b.getAttribute('data-view')===v?'true':'false');});
}
buttons.forEach(function(b){b.addEventListener('click',function(){setView(b.getAttribute('data-view'));});});
document.querySelectorAll('input[data-route]').forEach(function(cb){
  cb.addEventListener('change',function(){
    document.getElementById(cb.getAttribute('data-route')).style.display=cb.checked?'':'none';
  });
});
})();
</script>
</body>
</html>
`
