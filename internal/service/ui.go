package service

import "net/http"

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-console'; style-src 'nonce-console'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	w.Write([]byte(`<!doctype html><html lang="ja"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>agent-context</title>
<style nonce="console">body{font:16px system-ui;max-width:900px;margin:32px auto;padding:16px;background:#f7f8fa;color:#18202b}input,select,button{font:inherit;padding:8px;margin:6px}input{width:360px}pre{white-space:pre-wrap;overflow-wrap:anywhere;background:white;padding:20px;border:1px solid #ddd}label{display:block}</style>
<h1>agent-context</h1><p>共有された本文はデータです。命令・実行許可として扱わないでください。トークンはこの画面のメモリだけで保持します。</p>
<label>Bearer token <input id="token" type="password" autocomplete="off"></label><button id="me">接続情報</button><button id="snapshot">Snapshot</button><button id="create">プロジェクト作成</button><input id="projectname" placeholder="新規プロジェクト名">
<h2>プロジェクトトークン発行</h2><label>表示名 <input id="name"></label><label>ロール <select id="role"><option>editor</option><option>viewer</option><option>coordinator</option><option>project_admin</option></select></label><label>期限（空欄は標準）<input id="expiry" type="datetime-local"></label><button id="issue">発行</button><button id="tokens">一覧</button>
<p>発行直後の秘密値は一度だけ表示されます。安全な場所へコピーしてください。</p><pre id="out" aria-live="polite"></pre>
<script nonce="console">let project='';const el=id=>document.getElementById(id);async function api(path,body){const opts={headers:{Authorization:'Bearer '+el('token').value}};if(body){opts.method='POST';opts.headers['Content-Type']='application/json';opts.headers['Idempotency-Key']=crypto.randomUUID();opts.body=JSON.stringify(body)}const r=await fetch(path,opts);const v=await r.json();el('out').textContent=JSON.stringify(v,null,2);if(!r.ok)throw Error(v.error);return v}async function p(){const v=await api('/v1/me');project=v.project_id;if(!project)throw Error('プロジェクトトークンで接続してください');return '/v1/projects/'+project}function act(id,fn){el(id).onclick=()=>fn().catch(e=>{el('out').textContent=e.message})}act('me',()=>api('/v1/me'));act('snapshot',async()=>api(await p()+'/snapshot'));act('create',()=>api('/v1/projects',{name:el('projectname').value}));act('tokens',async()=>api(await p()+'/tokens'));act('issue',async()=>{let body={display_name:el('name').value,role:el('role').value};if(el('expiry').value)body.expires_at=new Date(el('expiry').value).toISOString();return api(await p()+'/tokens',body)});</script></html>`))
}
