package main

// Proxio — web proxy (CroxyProxy-style) in one file, stdlib only.
//
//	/p/https/example.com/path?q=1  ->  https://example.com/path?q=1
//
// URL scheme stays inside the path so relative URLs resolve by themselves;
// root-relative + absolute URLs in HTML/CSS get rewritten, cookies get
// re-scoped to the site's proxy path, and CSP/XFO/HSTS are dropped so the
// page can render inside the proxy.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const prefix = "/p/"

const version = "0.9.4"

// siteCookie remembers which site the browser is browsing so stray
// root-relative requests (JS-built "/api/x") can still be proxied.
const siteCookie = "proxio_site"

// swMarker flags a service-worker script so the server knows to inject swShimJS.
const swMarker = "__proxio_sw"

// workerMarker does the same for dedicated/shared workers (they have their own
// fetch/XHR, so an absolute API base inside a worker leaves the proxy).
const workerMarker = "__proxio_worker"

type ctxKey int

const swCtx ctxKey = iota

// shimJS is injected before any site script. It maps every URL the page
// builds at runtime (fetch/XHR/WebSocket/EventSource/beacon/history/open)
// back into the proxy, so SPAs keep talking through us.
const shimJS = `(function(){if(window.__proxio)return;window.__proxio=1;
var P='/p/';
function site(){var m=/^\/p\/([^\/]+)\/([^\/]+)/.exec(location.pathname);if(m)return m[1]+'/'+m[2];
var e=document.querySelector('meta[name=proxio-site]');return e&&e.content?e.content:null;}
function px(u){
try{
if(u==null)return u;
if(u instanceof URL)u=u.href;
if(typeof u!=='string'||!u)return u;
if(u.charAt(0)==='#'||u.indexOf(P)===0)return u;
if(/^(data|blob|javascript|mailto|tel|about|chrome-extension|filesystem):/i.test(u))return u;
var a=new URL(u,document.baseURI);
var s=site();
if(s){var i=a.pathname.indexOf(P+s);if(i>0)return a.pathname.slice(i)+a.search+a.hash;}
if(a.pathname.indexOf(P)===0)return u;
if(a.origin===location.origin){var h=s;if(!h)return u;return P+h+a.pathname+a.search+a.hash;}
if(/^(https?|wss?):$/.test(a.protocol))return P+a.protocol.slice(0,-1)+'/'+a.host+a.pathname+a.search+a.hash;
}catch(e){}
return u;}
function syncBase(){var b=document.querySelector('base[data-proxio]');if(b)b.href=location.href;}
var of=window.fetch;window.fetch=function(i,o){try{
if(typeof i==='string')i=px(i);
else if(i instanceof URL)i=px(i.href);
else if(i&&i.url){var n=px(i.url);if(n!==i.url)i=new Request(n,i);}
}catch(e){}
try{var t=new URL(typeof i==='string'?i:(i&&i.url)||'',document.baseURI);
if(/^https?:$/.test(t.protocol)&&t.origin!==location.origin)console.warn('[proxio] request keluar proxy:',t.href,new Error().stack);}catch(e){}
return of.call(this,i,o);};
if(window.XMLHttpRequest){var xo=XMLHttpRequest.prototype.open;
XMLHttpRequest.prototype.open=function(){var a=[].slice.call(arguments);try{if(a.length>1)a[1]=px(a[1]);}catch(e){}
try{var t=new URL(String(a[1]),document.baseURI);
if(/^https?:$/.test(t.protocol)&&t.origin!==location.origin)console.warn('[proxio] XHR keluar proxy:',t.href,new Error().stack);}catch(e){}
return xo.apply(this,a);};}
if(window.WebSocket){var W=window.WebSocket;var WP=function(u,p){return p?new W(px(u),p):new W(px(u));};
WP.prototype=W.prototype;['CONNECTING','OPEN','CLOSING','CLOSED'].forEach(function(k){WP[k]=W[k];});window.WebSocket=WP;}
if(window.EventSource){var E=window.EventSource;var EP=function(u,c){return new E(px(u),c);};EP.prototype=E.prototype;window.EventSource=EP;}
['Worker','SharedWorker'].forEach(function(k){var C=window[k];if(!C)return;
var F=function(u,o){try{var n=px(u);n+=(n.indexOf('?')<0?'?':'&')+'` + workerMarker + `=1';return new C(n,o);}catch(e){return new C(u,o);}};
F.prototype=C.prototype;window[k]=F;});
if(navigator.sendBeacon){var sb=navigator.sendBeacon.bind(navigator);navigator.sendBeacon=function(u,d){return sb(px(u),d);};}
['pushState','replaceState'].forEach(function(k){var o=history[k];history[k]=function(s,t,u){
var a=[].slice.call(arguments);if(a.length>2&&a[2]!=null)a[2]=px(a[2]);
var r=o.apply(this,a);syncBase();return r;};});
addEventListener('popstate',syncBase);
var wo=window.open;window.open=function(u){if(u)arguments[0]=px(u);return wo.apply(this,arguments);};
var sa=Element.prototype.setAttribute;Element.prototype.setAttribute=function(n,v){
if(typeof v==='string'&&(n==='href'||n==='src'||n==='action'||n==='poster'))v=px(v);
return sa.call(this,n,v);};
document.addEventListener('click',function(e){var a=e.target&&e.target.closest?e.target.closest('a[href]'):null;
if(!a)return;var h=a.getAttribute('href');if(!h)return;var n=px(h);if(n!==h)a.setAttribute('href',n);},true);
if(navigator.serviceWorker&&navigator.serviceWorker.register){
var rg=navigator.serviceWorker.register.bind(navigator.serviceWorker);
navigator.serviceWorker.register=function(u,o){
var n=px(u);n+=(n.indexOf('?')<0?'?':'&')+'` + swMarker + `=1';
if(o&&o.scope)o=Object.assign({},o,{scope:px(o.scope)});
return rg(n,o);};}
if(navigator.serviceWorker&&navigator.serviceWorker.getRegistrations){try{
navigator.serviceWorker.getRegistrations().then(function(rs){rs.forEach(function(r){
if(r.scope.indexOf(location.origin+P)!==0)r.unregister();});}).catch(function(){});}catch(e){}}
// a page that reached us at a bare path (old cache, typed URL) goes canonical
(function(){var s=site();if(!s)return;var p=location.pathname;if(p.indexOf(P)===0)return;
var i=p.indexOf(P+s);
location.replace(i>0?p.slice(i)+location.search+location.hash:P+s+p+location.search+location.hash);})();
// Cloudflare Turnstile/challenge cannot run through a rewriting proxy: its
// api.js looks for its own <script> tag by the real URL. Say so instead of
// leaving a cryptic TurnstileError in the console.
(function(){var done=0;
function real(){var m=/^\/p\/([^\/]+)\/([^\/]+)(\/.*)?$/.exec(location.pathname);
return m?m[1]+'://'+m[2]+(m[3]||'/')+location.search:null;}
function flag(){if(done)return;done=1;var r=real();if(!r)return;
var d=document.createElement('div');
d.setAttribute('style','position:fixed;top:0;left:0;right:0;z-index:2147483647;background:#7f1d1d;color:#fff;font:13px/1.45 system-ui,sans-serif;padding:8px 12px;box-shadow:0 2px 8px rgba(0,0,0,.4)');
d.innerHTML='<b>Proxio:</b> widget Cloudflare (Turnstile/challenge) tidak bisa jalan lewat proxy rewriting. <a style="color:#fff;text-decoration:underline" href="'+r+'" target="_blank" rel="noopener">Buka '+r+' langsung</a>';
document.documentElement.appendChild(d);}
function scan(){var a=document.querySelectorAll('script[src]');
for(var i=0;i<a.length;i++)if(/challenges\.cloudflare\.com|cdn-cgi\/challenge-platform/.test(a[i].src))return flag();}
function node(n){return n&&n.tagName==='SCRIPT'&&n.src&&/challenges\.cloudflare\.com|cdn-cgi\/challenge-platform/.test(n.src);}
scan();if(window.MutationObserver)new MutationObserver(function(ms){for(var i=0;i<ms.length;i++){var an=ms[i].addedNodes||[];for(var j=0;j<an.length;j++)if(node(an[j]))return flag();}}).observe(document.documentElement,{childList:true,subtree:true});
})();
})();`
// swShimJS is prepended to a service-worker script (marked by swMarker) so the
// worker resolves URLs against the proxied origin too — without it every cache
// key and fetch inside the SW points at the bare site origin.
const swShimJS = `(function(){var P='/p/';var L=self.location.href;
function site(){var m=/^\/p\/([^\/]+)\/([^\/]+)/.exec(self.location.pathname);return m?m[1]+'/'+m[2]:null;}
function px(u){
try{
if(u==null)return u;
if(Array.isArray(u))return u.map(px);
if(u instanceof Request)return new Request(px(u.url),u);
if(u instanceof URL)u=u.href;
if(typeof u!=='string'||!u)return u;
if(u.charAt(0)==='#'||u.indexOf(P)===0)return u;
if(/^(data|blob|javascript|about|chrome-extension):/i.test(u))return u;
var a=new URL(u,L);
var s=site();
if(s){var i=a.pathname.indexOf(P+s);if(i>0)return a.pathname.slice(i)+a.search+a.hash;}
if(a.pathname.indexOf(P)===0)return u;
if(a.origin===self.location.origin){var h=s;if(!h)return u;return P+h+a.pathname+a.search+a.hash;}
if(/^(https?|wss?):$/.test(a.protocol))return P+a.protocol.slice(0,-1)+'/'+a.host+a.pathname+a.search+a.hash;
}catch(e){}
return u;}
var of=self.fetch;self.fetch=function(i,o){return of.call(self,px(i),o);};
if(self.XMLHttpRequest){var xo=self.XMLHttpRequest.prototype.open;self.XMLHttpRequest.prototype.open=function(){var a=[].slice.call(arguments);if(a.length>1)a[1]=px(a[1]);return xo.apply(this,a);};}
if(self.WebSocket){var W= self.WebSocket;var WP=function(u,p){return p?new W(px(u),p):new W(px(u));};WP.prototype=W.prototype;self.WebSocket=WP;}
function wrap(o,k){var f=o&&o[k];if(typeof f!=='function')return;o[k]=function(){var a=[].slice.call(arguments);if(a.length)a[0]=px(a[0]);return f.apply(this,a);};}
['match','matchAll','add','addAll','put','delete'].forEach(function(k){wrap(Cache.prototype,k);wrap(CacheStorage.prototype,k);});
if(self.clients&&self.clients.openWindow){var ow=self.clients.openWindow.bind(self.clients);self.clients.openWindow=function(u){return ow(px(u));};}
self.__proxio_sw=1;
})();`


func proxyURL(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	s := prefix + u.Scheme + "/" + u.Host + p
	if u.RawQuery != "" {
		s += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		s += "#" + u.Fragment
	}
	return s
}

// parseTarget turns "/p/https/example.com/a?b=1" into a URL.
func parseTarget(raw string) (*url.URL, bool) {
	if !strings.HasPrefix(raw, prefix) {
		return nil, false
	}
	rest := raw[len(prefix):]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return nil, false
	}
	scheme, rest := rest[:i], rest[i+1:]
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	case "http", "https":
	default:
		return nil, false
	}
	host, path := rest, "/"
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		host, path = rest[:j], rest[j:]
	}
	if host == "" {
		return nil, false
	}
	u, err := url.Parse(scheme + "://" + host + path)
	if err != nil || u.Host == "" {
		return nil, false
	}
	return u, true
}

// strayTarget resolves a bare path (no /p/ prefix) against the site the browser
// is on: the bookkeeping cookie first, then the Referer (a page served from a
// service-worker cache carries no fresh cookie, but it still sends Referer).
func strayTarget(r *http.Request) (*url.URL, bool) {
	site := ""
	if c, err := r.Cookie(siteCookie); err == nil {
		site = c.Value
	}
	if site == "" {
		if ref, err := url.Parse(r.Header.Get("Referer")); err == nil && ref.Host != "" {
			if t, ok := parseTarget(ref.RequestURI()); ok {
				site = t.Scheme + "/" + t.Host
			}
		}
	}
	i := strings.IndexByte(site, '/')
	if i <= 0 {
		return nil, false
	}
	s, h := site[:i], site[i+1:]
	if (s != "http" && s != "https") || h == "" {
		return nil, false
	}
	u, err := url.Parse(s + "://" + h + r.URL.RequestURI())
	if err != nil || u.Host == "" {
		return nil, false
	}
	return u, true
}

// isNavigation reports whether the request is a document navigation (as opposed
// to an XHR/fetch, which must keep its method and body).
func isNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Sec-Fetch-Mode") == "navigate" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// isSWPath reports whether a path is a worker script (registration must not hang).
func isSWPath(p string) bool {
	p = strings.ToLower(p)
	return strings.HasSuffix(p, "/sw.js") || strings.HasSuffix(p, "service-worker.js")
}

// statusRecorder lets us log the status of a streamed proxy response.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) {
	if s.code == 0 {
		s.code = c
	}
	s.ResponseWriter.WriteHeader(c)
}

var attrRe = regexp.MustCompile(`(?i)\b(href|src|action|poster|formaction|data-src)=("|')([^"']*)("|')`)
var cssURLRe = regexp.MustCompile(`(?i)url\((['"]?)([^)'"]*)(['"]?)\)`)
var headRe = regexp.MustCompile(`(?i)<head[^>]*>`)
var baseRe = regexp.MustCompile(`(?i)<base[\s>/]`)

// rewriteRef maps a reference found in a document to its proxied form.
func rewriteRef(v string, base *url.URL) string {
	t := strings.TrimSpace(v)
	switch {
	case t == "", strings.HasPrefix(t, "#"), strings.HasPrefix(t, prefix):
		return v
	}
	for _, p := range []string{"data:", "javascript:", "mailto:", "tel:", "blob:", "about:", "chrome:"} {
		if strings.HasPrefix(strings.ToLower(t), p) {
			return v
		}
	}
	if strings.HasPrefix(t, "//") {
		t = "https:" + t
	}
	if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
		if u, err := url.Parse(t); err == nil && u.Host != "" {
			return proxyURL(u)
		}
		return v
	}
	if strings.HasPrefix(t, "/") {
		return prefix + base.Scheme + "/" + base.Host + t
	}
	return v // relative: resolves correctly under /p/scheme/host/ thanks to <base>
}

func rewriteCSS(b []byte, base *url.URL) []byte {
	return cssURLRe.ReplaceAllFunc(b, func(m []byte) []byte {
		s := cssURLRe.FindSubmatch(m)
		return []byte("url(" + string(s[1]) + rewriteRef(string(s[2]), base) + string(s[3]) + ")")
	})
}

func rewriteHTML(b []byte, base *url.URL) []byte {
	b = attrRe.ReplaceAllFunc(b, func(m []byte) []byte {
		s := attrRe.FindSubmatch(m)
		return []byte(string(s[1]) + "=" + string(s[2]) + rewriteRef(string(s[3]), base) + string(s[4]))
	})
	b = rewriteCSS(b, base)

	// <base> makes every relative URL resolve back into the proxy (skipped if
	// the page has its own); the shim runs before any site script so
	// runtime-built URLs get mapped too.
	tag := []byte(`<meta name="proxio-site" content="` + base.Scheme + "/" + base.Host + `">` +
		"<script>" + shimJS + "</script>")
	if !baseRe.Match(b) {
		tag = append([]byte(`<base data-proxio href="`+proxyURL(base)+`">`), tag...)
	}
	if loc := headRe.FindIndex(b); loc != nil {
		out := make([]byte, 0, len(b)+len(tag))
		out = append(out, b[:loc[1]]...)
		out = append(out, tag...)
		out = append(out, b[loc[1]:]...)
		return out
	}
	return append(tag, b...)
}

// rewriteManifest maps a PWA manifest's own URLs into the proxy, so
// start_url/scope/icons point back at us instead of the bare origin.
var manifestKeys = map[string]bool{"start_url": true, "scope": true, "id": true, "src": true, "url": true, "action": true}

func rewriteManifest(b []byte, base *url.URL) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	var walk func(any) any
	walk = func(n any) any {
		switch t := n.(type) {
		case map[string]any:
			for k, val := range t {
				if s, ok := val.(string); ok && manifestKeys[k] {
					t[k] = rewriteRef(s, base)
					continue
				}
				t[k] = walk(val)
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		}
		return n
	}
	walk(v)
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

// rewriteJS turns absolute references to the site's own origin into proxy paths.
// Without it a hardcoded API base (window.__CFG__ = {API: 'https://host/backend'})
// keeps calling the real origin from any context the page shim does not cover —
// old service workers, workers, popups — and those calls leave the proxy.
func rewriteJS(b []byte, base *url.URL) []byte {
	if len(b) > 2<<20 {
		return b
	}
	origin := base.Scheme + "://" + base.Host
	if !bytes.Contains(b, []byte(origin)) {
		return b
	}
	return bytes.ReplaceAll(b, []byte(origin+"/"), []byte(prefix+base.Scheme+"/"+base.Host+"/"))
}

// injectSWShim prepends swShimJS, keeping a leading "use strict" prologue first
// so the worker script does not silently lose strict mode.
var useStrictRe = regexp.MustCompile(`^(?:\xEF\xBB\xBF)?\s*(?:"use strict"|'use strict');?`)

func injectSWShim(b []byte) []byte {
	shim := []byte(swShimJS)
	if loc := useStrictRe.FindIndex(b); loc != nil {
		out := make([]byte, 0, len(b)+len(shim))
		out = append(out, b[:loc[1]]...)
		out = append(out, shim...)
		return append(out, b[loc[1]:]...)
	}
	return append(shim, b...)
}

// fixCookie drops Domain/Secure/SameSite and re-scopes Path to the site prefix
// so cookies survive over plain http and don't leak between proxied sites.
func fixCookie(c string, base *url.URL) string {
	parts := strings.Split(c, ";")
	out := parts[:0]
	for _, p := range parts {
		t := strings.ToLower(strings.TrimSpace(p))
		switch {
		case strings.HasPrefix(t, "domain="), t == "secure", strings.HasPrefix(t, "samesite="), t == "partitioned":
			continue
		case strings.HasPrefix(t, "path="):
			out = append(out, "Path="+prefix+base.Scheme+"/"+base.Host+"/")
		default:
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}

// cfWall reports a Cloudflare bot challenge in a proxied response. Turnstile and
// the managed challenge only work when the browser itself talks to Cloudflare,
// so the honest answer is to hand the URL back instead of serving a dead page.
func cfWall(resp *http.Response, body []byte) bool {
	if resp.Header.Get("cf-mitigated") != "" {
		return true
	}
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusServiceUnavailable {
		return false
	}
	return bytes.Contains(body, []byte("cdn-cgi/challenge-platform")) ||
		bytes.Contains(body, []byte("challenges.cloudflare.com/turnstile")) ||
		bytes.Contains(body, []byte("Just a moment"))
}

func cfNoticePage(target string) []byte {
	u := htmlEscape(target)
	return []byte(`<!doctype html><meta charset="utf-8"><title>Butuh browser langsung</title>
<body style="font:16px/1.6 system-ui;padding:2rem;max-width:44em;background:#0d1117;color:#e6edf3">
<h3>Situs ini memasang Cloudflare challenge</h3>
<p style="color:#8b949e">Halaman ini tidak bisa dirender lewat proxy rewriting: Cloudflare memeriksa
TLS fingerprint, User-Agent, dan IP dari klien yang mengambil halaman — dan yang mengambilnya di sini
adalah server Go, bukan browser-mu. CAPTCHA/Turnstile hanya bisa diselesaikan browser langsung.</p>
<p><a style="color:#58a6ff" href="` + u + `">Buka ` + u + ` langsung</a></p>
<p style="color:#484f58;font-size:.85rem">Kalau mau tetap lewat proxio: jalankan dengan
<code>-forward :8081</code> dan arahkan host ini ke tunnel (lihat <a style="color:#58a6ff" href="/proxy.pac">/proxy.pac</a>).</p>
</body>`)
}

func modify(resp *http.Response) error {
	h := resp.Header
	base := resp.Request.URL
	for _, k := range []string{
		"Content-Security-Policy", "Content-Security-Policy-Report-Only", "X-Frame-Options",
		"Strict-Transport-Security", "Cross-Origin-Opener-Policy", "Cross-Origin-Embedder-Policy",
		"Cross-Origin-Resource-Policy", "Report-To", "NEL", "Accept-CH",
	} {
		h.Del(k)
	}
	if cs, ok := h["Set-Cookie"]; ok {
		for i := range cs {
			cs[i] = fixCookie(cs[i], base)
		}
	}
	if loc := h.Get("Location"); loc != "" {
		h.Set("Location", rewriteRef(loc, base))
	}

	ct := strings.ToLower(h.Get("Content-Type"))
	isHTML := strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml")
	isCSS := strings.Contains(ct, "text/css")
	isManifest := strings.Contains(ct, "manifest") || strings.HasSuffix(strings.ToLower(base.Path), ".webmanifest")
	isJS := strings.Contains(ct, "javascript") || strings.Contains(ct, "ecmascript")
	isSW := isJS && resp.Request.Context().Value(swCtx) != nil
	if !isHTML && !isCSS && !isManifest && !isJS || resp.Request.Method == "HEAD" {
		return nil
	}
	// big bundles are not worth buffering; they rarely hardcode their own origin
	if isJS && !isSW && resp.ContentLength > 2<<20 {
		return nil
	}

	if isHTML {
		h.Add("Set-Cookie", siteCookie+"="+base.Scheme+"/"+base.Host+"; Path=/; HttpOnly")
	}

	body := resp.Body
	if ce := strings.ToLower(h.Get("Content-Encoding")); ce != "" {
		if !strings.Contains(ce, "gzip") {
			return nil // br/zstd: pass through untouched
		}
		gz, err := gzip.NewReader(body)
		if err != nil {
			return nil
		}
		body = gz
		h.Del("Content-Encoding")
	}
	b, err := io.ReadAll(io.LimitReader(body, 16<<20))
	body.Close()
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		resp.ContentLength = 0
		return nil
	}
	switch {
	case isHTML:
		if cfWall(resp, b) && isNavigation(resp.Request) {
			b = cfNoticePage(base.String())
		} else {
			b = rewriteHTML(b, base)
		}
	case isManifest:
		b = rewriteManifest(b, base)
	case isCSS:
		b = rewriteCSS(b, base)
	case isJS:
		b = rewriteJS(b, base)
		if isSW {
			b = injectSWShim(b)
		}
	}
	h.Del("Content-Encoding")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	resp.Body = io.NopCloser(bytes.NewReader(b))
	resp.ContentLength = int64(len(b))
	return nil
}

func director(req *http.Request) {
	req.Host = ""
	req.Header.Del("Accept-Encoding")
	req.Header.Del("Proxy-Connection")
	// our own bookkeeping cookie must not reach the site
	if c := req.Header.Get("Cookie"); c != "" {
		keep := make([]string, 0, 8)
		for _, kv := range strings.Split(c, "; ") {
			if !strings.HasPrefix(kv, siteCookie+"=") {
				keep = append(keep, kv)
			}
		}
		if len(keep) == 0 {
			req.Header.Del("Cookie")
		} else {
			req.Header.Set("Cookie", strings.Join(keep, "; "))
		}
	}
	if req.Header.Get("Origin") != "" {
		req.Header.Set("Origin", req.URL.Scheme+"://"+req.URL.Host)
	}
	if ref := req.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil {
			if t, ok := parseTarget(u.RequestURI()); ok {
				req.Header.Set("Referer", t.String())
			}
		}
	}
}

func newProxy() *httputil.ReverseProxy {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		TLSClientConfig:       insecureTLS(),
	}
	return &httputil.ReverseProxy{
		Director:       director,
		ModifyResponse: modify,
		Transport:      tr,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy error %s: %v", r.URL, err)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `<body style="font:16px system-ui;padding:2rem;background:#111;color:#eee">
<h3>Gagal membuka situs</h3><p style="color:#f88">`+
				htmlEscape(err.Error())+`</p><a style="color:#7cf" href="/">Kembali</a></body>`)
		},
	}
}

// ponytail: TLS verify off so sites with broken/expired certs still load
// (that's the whole point of a public proxy). Add a verifier when you need pinning.
func insecureTLS() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

const homePage = `<!doctype html><html lang="id"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>Proxio</title>
<style>
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0d1117;color:#e6edf3;
font:16px/1.5 system-ui,Segoe UI,sans-serif}
main{width:min(680px,92vw);padding:2rem 0}
h1{font-size:2.2rem;margin:0 0 .3rem;letter-spacing:-.02em}
h1 span{color:#58a6ff}
p.sub{margin:0 0 1.6rem;color:#8b949e}
form{display:flex;gap:.5rem}
input{flex:1;padding:.9rem 1rem;border-radius:10px;border:1px solid #30363d;background:#161b22;color:#e6edf3;font-size:1rem}
input:focus{outline:2px solid #58a6ff;border-color:transparent}
button{padding:.9rem 1.4rem;border:0;border-radius:10px;background:#238636;color:#fff;font-weight:600;font-size:1rem;cursor:pointer}
button:hover{background:#2ea043}
ul.links{list-style:none;display:flex;flex-wrap:wrap;gap:.5rem;padding:0;margin:1.6rem 0 0}
ul.links a{color:#8b949e;text-decoration:none;border:1px solid #30363d;border-radius:999px;padding:.35rem .8rem;font-size:.85rem}
ul.links a:hover{color:#e6edf3;border-color:#58a6ff}
footer{margin-top:2rem;color:#484f58;font-size:.8rem}
</style></head><body><main>
<h1>Pro<span>xio</span></h1>
<p class="sub">Buka situs web apa pun lewat proxy. Tanpa iklan, tanpa tracking.</p>
<form action="/go" method="get">
<input name="u" placeholder="example.com" autocomplete="off" autofocus>
<button>Buka</button>
</form>
<ul class="links">
<li><a href="/p/https/wikipedia.org/">Wikipedia</a></li>
<li><a href="/p/https/news.ycombinator.com/">Hacker News</a></li>
<li><a href="/p/https/example.com/">example.com</a></li>
</ul>
<footer>Proxio &mdash; Go net/http reverse proxy. Konten diambil langsung dari sumber aslinya. &middot; <a style="color:#484f58" href="/__reset">reset SW/cache</a> &middot; <a style="color:#484f58" href="/proxy.pac">proxy.pac</a></footer>
</main></body></html>`

const resetPage = `<!doctype html><meta charset="utf-8"><title>Reset Proxio</title>
<body style="font:16px system-ui;padding:2rem;background:#0d1117;color:#e6edf3">
<h3>Reset service worker &amp; cache</h3>
<pre id="log" style="background:#161b22;padding:1rem;border-radius:8px;white-space:pre-wrap"></pre>
<p><a style="color:#58a6ff" href="/">Kembali ke beranda</a></p>
<script>
var out = [];
function p(s){ out.push(s); document.getElementById('log').textContent = out.join('\n'); }
var c = navigator.serviceWorker && navigator.serviceWorker.controller;
p('controller: ' + (c ? c.scriptURL : '(tidak ada)'));
var jobs = [];
if (navigator.serviceWorker) {
  jobs.push(navigator.serviceWorker.getRegistrations().then(function(rs){
    p('registrasi ditemukan: ' + rs.length);
    return Promise.all(rs.map(function(r){
      var sc = r.scope;
      return r.unregister().then(function(ok){ p((ok ? 'unregistered ' : 'GAGAL ') + sc); });
    }));
  }));
}
if (window.caches) {
  jobs.push(caches.keys().then(function(ks){
    p('cache ditemukan: ' + ks.length);
    return Promise.all(ks.map(function(k){
      return caches.delete(k).then(function(ok){ p((ok ? 'deleted ' : 'GAGAL ') + k); });
    }));
  }));
}
Promise.all(jobs).then(function(){
  p('selesai — reload halaman situs untuk memastikan.');
}).catch(function(e){ p('ERROR: ' + e); });
</script></body>`

const notFoundPage = `<!doctype html><meta charset="utf-8"><title>Proxio — tidak bisa dipetakan</title>
<body style="font:16px/1.6 system-ui;padding:2rem;max-width:46em;background:#0d1117;color:#e6edf3">
<h3>Path ini tidak bisa dipetakan ke situs mana pun</h3>
<p style="color:#8b949e">Proxy hanya mengenal dua bentuk URL:</p>
<pre style="background:#161b22;padding:1rem;border-radius:8px;overflow:auto">/p/https/example.com/halaman   (lewat proxy)
/go?u=example.com              (redirect helper)</pre>
<p style="color:#8b949e">Kalau kamu melihat request seperti ini di DevTools padahal sedang membuka situs
lewat proxy, penyebabnya biasanya <b>service worker lama</b> yang terdaftar dari build
sebelumnya (scope-nya masih di path tanpa <code>/p/</code>) dan sedang mengecek update
skripnya. Hapus dulu:</p>
<p><a style="color:#58a6ff" href="/__reset">Reset service worker &amp; cache</a> &middot;
<a style="color:#58a6ff" href="/">Kembali ke beranda</a></p>
</body>`

// pingHandler fetches a URL server-side and reports what came back — the fastest
// way to tell "upstream hangs" from "proxy hangs".
var pingClient = &http.Client{
	Timeout:   20 * time.Second,
	Transport: &http.Transport{Proxy: nil, DisableCompression: true, TLSClientConfig: insecureTLS()},
}

func pingHandler(w http.ResponseWriter, r *http.Request) {
	u := strings.TrimSpace(r.URL.Query().Get("u"))
	if u == "" {
		http.Error(w, "pakai /__ping?u=https://host/path", http.StatusBadRequest)
		return
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	tgt, err := url.Parse(u)
	if err != nil || tgt.Host == "" {
		http.Error(w, "URL tidak valid", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	start := time.Now()
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, tgt.String(), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (proxio ping)")
	resp, err := pingClient.Do(req)
	if err != nil {
		fmt.Fprintf(w, "GAGAL setelah %dms: %v\n", time.Since(start).Milliseconds(), err)
		return
	}
	defer resp.Body.Close()
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	fmt.Fprintf(w, "%s %s\n%dms\ncontent-type: %s\ncontent-length: %d\n\n%s\n",
		resp.Proto, resp.Status, time.Since(start).Milliseconds(),
		resp.Header.Get("Content-Type"), resp.ContentLength, head)
}

// fwdClient handles the plain-http (absolute-form) half of forward-proxy mode.
var fwdClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		Proxy:               nil,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     insecureTLS(),
		MaxIdleConnsPerHost: 16,
	},
}

// forwardProxy is a plain CONNECT tunnel: the browser keeps the real origin and
// does its own TLS, so Cloudflare sees a real browser fingerprint and a CAPTCHA
// (or any SSO flow) works the way it does without a proxy. No rewriting happens
// here — that is the point: rewriting proxies cannot pass bot challenges.
func forwardProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		// plain-http proxying: browsers send an absolute-form GET/POST
		if r.URL == nil || !r.URL.IsAbs() {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		resp, err := fwdClient.Do(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	up, err := net.DialTimeout("tcp", r.Host, 15*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	c, _, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		c.Close()
		up.Close()
		return
	}
	go func() { io.Copy(up, c); up.Close() }()
	io.Copy(c, up)
	c.Close()
}

// pacHost is the address the PAC should point the browser at: whatever host it
// used to fetch the PAC, so it also works from another machine or from Docker.
func pacHost(r *http.Request) string {
	h := r.Host
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	switch h {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	return h
}

// pacScript returns a PAC that tunnels the listed hosts through the CONNECT
// forward proxy (browser does TLS itself: SSO/Cloudflare work natively) and lets
// everything else go direct.
func pacScript(host, port string, hosts []string) string {
	var b strings.Builder
	b.WriteString("function FindProxyForURL(url, host) {\n  var h = host.toLowerCase();\n")
	for _, d := range hosts {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if d == "*" {
			fmt.Fprintf(&b, "  return \"PROXY %s:%s\";\n}\n", host, port)
			return b.String()
		}
		fmt.Fprintf(&b, "  if (h === %q || shExpMatch(h, \"*.%s\")) return \"PROXY %s:%s\";\n", d, d, host, port)
	}
	b.WriteString("  return \"DIRECT\";\n}\n")
	return b.String()
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	fwd := flag.String("forward", "", "optional CONNECT (forward proxy) listen address, e.g. :8081")
	fwdHosts := flag.String("fwd-hosts", "bps.go.id", "comma-separated hosts tunnelled by /proxy.pac (suffix match, * = all)")
	flag.Parse()

	if *fwd != "" {
		go func() {
			log.Printf("proxio %s forward proxy on http://localhost%s (CONNECT only)", version, *fwd)
			err := http.ListenAndServe(*fwd, http.HandlerFunc(forwardProxy))
			log.Printf("forward proxy stopped: %v", err)
		}()
	}

	rp := newProxy()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, homePage)
			return
		case r.URL.Path == "/go":
			u := strings.TrimSpace(r.URL.Query().Get("u"))
			if u == "" {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
			if !strings.Contains(u, "://") {
				u = "https://" + u
			}
			t, err := url.Parse(u)
			if err != nil || t.Host == "" || (t.Scheme != "http" && t.Scheme != "https") {
				http.Error(w, "URL tidak valid", http.StatusBadRequest)
				return
			}
			if t.Path == "" {
				t.Path = "/"
			}
			http.Redirect(w, r, proxyURL(t), http.StatusFound)
			return
		case r.URL.Path == "/favicon.ico":
			w.WriteHeader(http.StatusNoContent)
			return
		case r.URL.Path == "/__reset":
			// "storage" also drops service-worker registrations; belt and braces
			// for workers that are stuck outside the proxy scope.
			w.Header().Set("Clear-Site-Data", `"cache", "storage"`)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, resetPage)
			return
		case r.URL.Path == "/__version":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			io.WriteString(w, version+"\n")
			return
		case r.URL.Path == "/__ping":
			pingHandler(w, r)
			return
		case r.URL.Path == "/proxy.pac":
			w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
			port := ""
			if _, p, err := net.SplitHostPort(*fwd); err == nil {
				port = p
			}
			if port == "" {
				io.WriteString(w, "function FindProxyForURL(url, host) { return \"DIRECT\"; }\n")
				return
			}
			io.WriteString(w, pacScript(pacHost(r), port, strings.Split(*fwdHosts, ",")))
			return
		}

		t, ok := parseTarget(r.URL.RequestURI())
		if !ok {
			// stray root-relative request (JS-built "/api/x", a bare SW script):
			// assume the site the browser is on and proxy it inline.
			t, ok = strayTarget(r)
			if !ok {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, notFoundPage)
				return
			}
			// A page served at a bare path makes every relative URL, worker and
			// register() resolve outside /p/ — so navigations are sent to the
			// canonical proxied URL. XHR/fetch keep the inline proxy (POST body).
			if isNavigation(r) {
				http.Redirect(w, r, proxyURL(t), http.StatusFound)
				return
			}
			// before the shim existed (new ones always carry /p/). Wipe this
			// origin's storage so that stale registration dies for good.
			if p := strings.ToLower(t.Path); strings.HasSuffix(p, "/sw.js") || strings.HasSuffix(p, "service-worker.js") {
				w.Header().Set("Clear-Site-Data", `"storage"`)
			}
		}
		ctx := r.Context()
		// a worker script must never hang the registration: bound it
		if isSWPath(t.Path) {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
		}
		if t.RawQuery != "" {
			if q, err := url.ParseQuery(t.RawQuery); err == nil {
				if q.Has(swMarker) || q.Has(workerMarker) {
					q.Del(swMarker)
					q.Del(workerMarker)
					t.RawQuery = q.Encode()
					ctx = context.WithValue(ctx, swCtx, true)
				}
			}
		}
		r2 := r.Clone(ctx)
		r2.URL = t
		if isSWPath(t.Path) {
			rec := &statusRecorder{ResponseWriter: w}
			start := time.Now()
			rp.ServeHTTP(rec, r2)
			log.Printf("sw %s -> %d in %dms", t.String(), rec.code, time.Since(start).Milliseconds())
			return
		}
		rp.ServeHTTP(w, r2)
	})

	srv := &http.Server{
		Addr:              *addr,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("proxio %s listening on http://localhost%s", version, *addr)
	log.Fatal(srv.ListenAndServe())
}
