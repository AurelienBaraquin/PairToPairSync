// Package api serves a read-only status page on the loopback interface.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"
)

// InviteResult is what creating an invitation returns to the page.
type InviteResult struct {
	Code    string
	PNG     []byte
	Expires time.Time
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
<div class=card id=inv><b>Inviter un appareil</b><br><small>Affiche un QR code à usage unique (valable 10 min). Traitez-le comme un mot de passe.</small><br>
<select id=f></select> <label><input type=checkbox id=rl> cet appareil sera un <b>relais</b> (ex. téléphone)</label>
<button id=go>Générer le QR code</button><div id=res></div></div>
<script>
const fmt=b=>b>1e9?(b/1e9).toFixed(1)+' GB':b>1e6?(b/1e6).toFixed(1)+' MB':b>1e3?(b/1e3).toFixed(1)+' kB':b+' B';
async function r(){try{const s=await (await fetch('/api/status')).json();
let h='<p><b>'+s.device_name+'</b><br><small>ID: <code>'+s.device_id+'</code></small></p>';
for(const f of s.folders){const rl=f.role==='relay';
h+='<div class=card><b>'+(f.label||f.id)+'</b><span class="tag '+(rl?'relay':'')+'">'+(rl?'relais temporaire':'copie complète')+'</span>'+
'<span class=tag>'+f.state+'</span><br><small>'+f.path+'</small><br>'+
(rl?f.pending_changes+' modification(s) en attente · '+fmt(f.stored_bytes)+' / '+fmt(f.quota_bytes)+(f.waiting_for_devices?'<br><small>en attente de '+f.waiting_for_devices.length+' appareil(s)</small>':'')
:f.local_files+' fichiers · '+fmt(f.local_bytes)+(f.need_files?' · <b>'+f.need_files+' à synchroniser</b> ('+fmt(f.need_bytes)+')':' · à jour'))+
(f.error?'<br><b style=color:#b00>'+f.error+'</b>':'')+
'<br><small>connectés : '+((f.connected_peers||[]).join(', ')||'aucun')+'</small></div>'}
document.getElementById('o').innerHTML=h}catch(e){document.getElementById('o').textContent='Démon injoignable'}}
r();setInterval(r,2000)
let loaded=false;
async function folders(){try{const s=await (await fetch('/api/status')).json();const sel=document.getElementById('f');
sel.innerHTML=s.folders.map(f=>'<option value="'+f.id+'" data-role="'+f.role+'">'+(f.label||f.id)+' ('+(f.role==='relay'?'relais':'complet')+')</option>').join('')}catch(e){}}
folders();
document.getElementById('go').onclick=async()=>{const res=document.getElementById('res');res.textContent='…';
const sel=document.getElementById('f');
try{const r=await fetch('/api/invite',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({folder:sel.value,as_relay:document.getElementById('rl').checked})});
const j=await r.json();if(!r.ok){res.textContent=j.error||'refusé';return}
res.innerHTML='<p><img src="'+j.qr+'" width=260 height=260 alt="QR code"></p><p><small>Sur l\'autre appareil : <code>p2psync pair &lt;code&gt;</code> ou scannez ce QR.<br>Expire à '+new Date(j.expires*1000).toLocaleTimeString()+'.</small></p><textarea readonly rows=3 style="width:100%;font-size:.7rem">'+j.code+'</textarea>'}
catch(e){res.textContent='Erreur : '+e}};
</script>`

// Start listens on addr (loopback only unless the user chooses otherwise).
func Start(addr string, status func() any, invite func(folder string, asRelay bool) (InviteResult, error), log *slog.Logger) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	s := &Server{ln: ln, srv: &http.Server{Handler: localHostOnly(mux), ReadHeaderTimeout: 5 * time.Second}}
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
