// Package api serves a read-only status page on the loopback interface.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// InviteResult is what creating an invitation returns to the page.
type InviteResult struct {
	Code    string
	PNG     []byte
	Expires time.Time
}

// Hooks are what the page can ask the daemon to do.
type Hooks struct {
	Status    func() any
	Invite    func(folder string, asRelay bool) (InviteResult, error)
	NetInvite func(folders []string, asRelay, admin, approve bool) (InviteResult, error)
	Requests  func() any // devices waiting for this admin's decision
	Decide    func(id string, accept bool) bool
}

type Server struct {
	srv *http.Server
	ln  net.Listener
}

// Addr is the address the server listens on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

const page = `<!doctype html><meta charset="utf-8"><title>PairToPairSync</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font:15px system-ui;margin:2rem auto;max-width:48rem;padding:0 1rem;color:#222}
h1{font-size:1.3rem}.card{border:1px solid #ddd;border-radius:8px;padding:.8rem 1rem;margin:.7rem 0}
.tag{font-size:.75rem;background:#eef;border-radius:4px;padding:1px 6px;margin-left:.4rem}
.relay{background:#ffe9c7}small{color:#666}code{word-break:break-all}</style>
<h1>PairToPairSync</h1><div id=o>…</div>
<div class=card id=inv hidden><b>Inviter un appareil</b><br><small>Affiche un QR code à usage unique (valable 10 min). Traitez-le comme un mot de passe.</small><br>
<select id=f></select> <label><input type=checkbox id=rl> cet appareil sera un <b>relais</b> (ex. téléphone)</label>
<button id=go>Générer le QR code</button><div id=res></div></div>
<div class=card id=nw hidden><b>Inviter dans le réseau (administrateur)</b><br><small>Le nouvel appareil se connecte à vous seul : vous le présentez aux appareils des dossiers choisis. Vous confirmez chaque demande ci-dessous.</small><br>
<div id=nf></div><label><input type=checkbox id=nrl> relais (ex. téléphone)</label> <label><input type=checkbox id=nad> administrateur</label>
<button id=ngo>Générer le QR code</button><div id=nres></div></div>
<div class=card id=rq hidden><b>Demandes de connexion</b><div id=rql></div></div>
<script src="/app.js"></script>`

// appJS is served as a separate file so the Content-Security-Policy can forbid
// inline scripts. Every value coming from the daemon is inserted as text
// (textContent), never as markup: names and labels can originate from other
// devices.
const appJS = `
const fmt=b=>b>1e9?(b/1e9).toFixed(1)+' GB':b>1e6?(b/1e6).toFixed(1)+' MB':b>1e3?(b/1e3).toFixed(1)+' kB':b+' B';
const $=id=>document.getElementById(id);
function el(tag,text,cls){const e=document.createElement(tag);if(text!==undefined)e.textContent=text;if(cls)e.className=cls;return e}
// The access token travels in the URL fragment (never sent to a server); keep it for this tab only.
let token=sessionStorage.getItem('t');
if(location.hash.startsWith('#t=')){token=location.hash.slice(3);sessionStorage.setItem('t',token);history.replaceState(null,'',location.pathname)}
const hdr=()=>({'Authorization':'Bearer '+token});
async function status(){const r=await fetch('/api/status',{headers:hdr()});if(!r.ok)throw new Error(r.status);return r.json()}
async function render(){const o=$('o');
 try{const s=await status();const out=document.createDocumentFragment();
  const head=el('p');head.append(el('b',s.device_name),el('br'),el('small','ID : '+s.device_id));out.append(head);
  for(const f of s.folders){const rl=f.role==='relay';const c=el('div',undefined,'card');
   c.append(el('b',f.label||f.id),el('span',rl?'relais temporaire':'copie complète','tag'+(rl?' relay':'')),el('span',f.state,'tag'),el('br'),el('small',f.path),el('br'));
   if(rl){c.append(f.pending_changes+' modification(s) en attente · '+fmt(f.stored_bytes)+' / '+fmt(f.quota_bytes))}
   else{c.append(f.local_files+' fichiers · '+fmt(f.local_bytes)+' · '+(f.need_files?f.need_files+' à synchroniser ('+fmt(f.need_bytes)+')':'à jour'))}
   if(f.error){c.append(el('br'),el('b',f.error))}
   c.append(el('br'),el('small','connectés : '+((f.connected_peers||[]).join(', ')||'aucun')));out.append(c)}
  o.replaceChildren(out);
  const sel=$('f');if(sel.options.length!==s.folders.length){sel.replaceChildren(...s.folders.map(f=>{const op=el('option',(f.label||f.id)+' ('+(f.role==='relay'?'relais':'complet')+')');op.value=f.id;return op}))}
  $('inv').hidden=false}
 catch(e){o.textContent=token?'Démon injoignable ou accès refusé.':'Ouvrez cette page avec le lien donné par « p2psync ui » (il contient le jeton d’accès).'}}
const post=(url,body)=>fetch(url,{method:'POST',headers:{'Content-Type':'application/json',...hdr()},body:JSON.stringify(body)});
async function requests(){try{const s=await status();$('nw').hidden=!s.admin;
  const nf=$('nf');if(s.admin&&nf.childElementCount!==s.folders.length){nf.replaceChildren(...s.folders.filter(f=>!f.managed).map(f=>{const l=el('label');const c=el('input');c.type='checkbox';c.value=f.id;c.checked=true;l.append(c,' '+(f.label||f.id)+' ');return l}))}
  const rs=s.requests||[];$('rq').hidden=!rs.length;
  $('rql').replaceChildren(...rs.map(r=>{const d=el('div',undefined,'card');
   d.append(el('b',r.name||'(sans nom)'),el('br'),el('small','ID : '+r.device_id+(r.ip?' · '+r.ip:'')),el('br'),
    el('span','dossiers : '+(r.folders||[]).join(', ')+(r.as_relay?' · relais':'')+(r.admin?' · administrateur':'')),el('br'));
   for(const [label,ok] of [['Accepter',true],['Refuser',false]]){const b=el('button',label);b.onclick=async()=>{await post('/api/requests/decide',{id:r.id,accept:ok});requests()};d.append(b,' ')}
   return d}))}catch(e){}}
$('ngo').onclick=async()=>{const res=$('nres');res.textContent='…';
 try{const folders=[...$('nf').querySelectorAll('input:checked')].map(c=>c.value);
  const r=await post('/api/network-invite',{folders,as_relay:$('nrl').checked,admin:$('nad').checked,approve:true});
  const j=await r.json();if(!r.ok){res.textContent=j.error||'refusé';return}
  const img=el('img');img.src=j.qr;img.width=260;img.height=260;img.alt='QR code';
  const ta=el('textarea',j.code);ta.readOnly=true;ta.rows=3;ta.style.cssText='width:100%;font-size:.7rem';
  res.replaceChildren(img,el('p','Sur l’autre appareil : p2psync pair <texte> --dir <dossier>, ou scannez ce QR. Expire à '+new Date(j.expires*1000).toLocaleTimeString()+'.','small'),ta)}
 catch(e){res.textContent='Erreur : '+e}};
render();setInterval(render,2000);requests();setInterval(requests,2000);
$('go').onclick=async()=>{const res=$('res');res.textContent='…';
 try{const r=await fetch('/api/invite',{method:'POST',headers:{'Content-Type':'application/json',...hdr()},body:JSON.stringify({folder:$('f').value,as_relay:$('rl').checked})});
  const j=await r.json();if(!r.ok){res.textContent=j.error||'refusé';return}
  const img=el('img');img.src=j.qr;img.width=260;img.height=260;img.alt='QR code';
  const ta=el('textarea',j.code);ta.readOnly=true;ta.rows=3;ta.style.cssText='width:100%;font-size:.7rem';
  res.replaceChildren(img,el('p','Sur l’autre appareil : p2psync pair <texte>, ou scannez ce QR. Expire à '+new Date(j.expires*1000).toLocaleTimeString()+'.','small'),ta)}
 catch(e){res.textContent='Erreur : '+e}};
`

// Start listens on addr (loopback only unless the user chooses otherwise).
func Start(addr, token string, h Hooks, log *slog.Logger) (*Server, error) {
	status, invite := h.Status, h.Invite
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status())
	})
	mux.HandleFunc("/api/invite", func(w http.ResponseWriter, r *http.Request) {
		// This endpoint creates a credential, so it must not be triggerable by a
		// web page the user happens to visit: require a same-origin JSON POST.
		if r.Method != http.MethodPost || !sameOrigin(r) || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Folder  string `json:"folder"`
			AsRelay bool   `json:"as_relay"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		res, err := invite(req.Folder, req.AsRelay)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": res.Code, "expires": res.Expires.Unix(),
			"qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(res.PNG),
		})
	})
	// Everything below changes state or creates a credential: same-origin JSON POST, token required.
	guarded := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || !sameOrigin(r) || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/api/network-invite", func(w http.ResponseWriter, r *http.Request) {
		if !guarded(w, r) || h.NetInvite == nil {
			return
		}
		var req struct {
			Folders []string `json:"folders"`
			AsRelay bool     `json:"as_relay"`
			Admin   bool     `json:"admin"`
			Approve bool     `json:"approve"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		res, err := h.NetInvite(req.Folders, req.AsRelay, req.Admin, req.Approve)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": res.Code, "expires": res.Expires.Unix(),
			"qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(res.PNG),
		})
	})
	mux.HandleFunc("/api/requests", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) || h.Requests == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.Requests())
	})
	mux.HandleFunc("/api/requests/decide", func(w http.ResponseWriter, r *http.Request) {
		if !guarded(w, r) || h.Decide == nil {
			return
		}
		var req struct {
			ID     string `json:"id"`
			Accept bool   `json:"accept"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !h.Decide(req.ID, req.Accept) {
			http.Error(w, "no such request", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(appJS))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	s := &Server{ln: ln, srv: &http.Server{Handler: secureHeaders(localHostOnly(mux)), ReadHeaderTimeout: 5 * time.Second}}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Warn("status API stopped", "err", err)
		}
	}()
	return s, nil
}

func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
}

// sameOrigin accepts only requests issued by our own page.
func sameOrigin(r *http.Request) bool {
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// localHostOnly defeats DNS rebinding: a page on evil.example whose name was
// re-pointed to 127.0.0.1 would send "Host: evil.example", which we refuse.
func localHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		switch host {
		case "localhost", "127.0.0.1", "[::1]", "::1":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
		}
	})
}

// authorized checks the bearer token in constant time. The token lives in a
// 0600 file in the user's config directory, so another account on the same
// machine cannot read the status or create invitations through this port.
func authorized(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// secureHeaders forbids inline script, framing and content sniffing, so that a
// bug in escaping cannot become code execution on this origin.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
