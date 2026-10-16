// Package api serves a read-only status page on the loopback interface.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type Server struct{ srv *http.Server }

const page = `<!doctype html><meta charset="utf-8"><title>PairToPairSync</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font:15px system-ui;margin:2rem auto;max-width:48rem;padding:0 1rem;color:#222}
h1{font-size:1.3rem}.card{border:1px solid #ddd;border-radius:8px;padding:.8rem 1rem;margin:.7rem 0}
.tag{font-size:.75rem;background:#eef;border-radius:4px;padding:1px 6px;margin-left:.4rem}
.relay{background:#ffe9c7}small{color:#666}code{word-break:break-all}</style>
<h1>PairToPairSync</h1><div id=o>…</div>
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
</script>`

// Start listens on addr (loopback only unless the user chooses otherwise).
func Start(addr string, status func() any, log *slog.Logger) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	s := &Server{srv: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}}
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
