# Proxio

Web proxy, satu file Go, stdlib (tanpa dependency).

## Jalan

```bash
go run . -addr :8080
```

Buka http://localhost:8080, ketik domain, klik Buka. Atau langsung:

```
http://localhost:8080/p/https/example.com/halaman?q=1
http://localhost:8080/go?u=example.com          # redirect helper
```

## Cara kerja

| Bagian | Isi |
|---|---|
| URL | `/p/<scheme>/<host>/<path>` — relative URL resolve sendiri karena prefix |
| HTML | `<base href>` disuntik + atribut `href/src/action/poster/formaction` dan `url(...)` di-rewrite |
| CSS | `url(...)` di-rewrite, relative aman lewat base |
| Redirect | header `Location` dipetakan balik ke bentuk proxy |
| Cookie | `Domain`/`Secure`/`SameSite` dibuang, `Path` di-scope ke `/p/<scheme>/<host>/` |
| Header | CSP, X-Frame-Options, HSTS, COOP/COEP/CORP dihapus supaya halaman bisa dirender |
| Transport | gzip di-decode saat rewrite, TLS verify off (situs cert rusak tetap jalan), WebSocket lewat (Upgrade) |
| SPA | shim JS disuntik sebelum script situs: `fetch`/`XHR`/`WebSocket`/`EventSource`/`sendBeacon`/`pushState`/`window.open` di-rewrite, `<base>` ikut rute SPA |
| JS | origin situs sendiri di dalam file JS (mis. `config.js`: `VITE_API_BASE_URL: 'https://host/backend'`) di-rewrite jadi `/p/https/host/backend`; file > 2 MB dilewati |
| Prefix ganda | `px()` menormalkan URL yang sudah mengandung `/p/<site>` di tengah path (router yang menempelkan base ke `location.pathname`) |
| Worker | `new Worker(...)`/`SharedWorker` dipetakan + ditandai `?__proxio_worker=1`; server menyuntik shim worker (fetch/XHR/WebSocket/Cache) |
| Detektor | shim `console.warn` kalau ada request yang tetap lintas-origin, lengkap dengan stack trace |
| CF challenge | halaman 403/503 + `cf-mitigated` diganti halaman penjelasan; kalau halaman menyuntik script `challenges.cloudflare.com` (Turnstile), muncul banner merah berisi link ke URL asli |
| PWA | manifest di-rewrite (`scope`, `start_url`, `icons`, `shortcuts`, `action`), service worker diizinkan, script SW disuntik shim sisi-worker |
| Path nyasar | navigasi (GET + `Accept: text/html` / `Sec-Fetch-Mode: navigate`) di-**302** ke URL kanonik `/p/...`; XHR/fetch diproxy inline (POST tidak berubah jadi GET). Situs ditebak dari cookie `proxio_site`, lalu header `Referer` |
| Diagnosa | `/__version` (build yang jalan), `/__reset` (hapus SW + cache), halaman 404 yang menjelaskan penyebab |

## SPA

Halaman yang mengambil data setelah load (React/Vue/Next/Algolia) tetap jalan:

- URL yang dibangun runtime (`fetch("/api")`, `location.origin + "/x"`, `new WebSocket("wss://...")`) dipetakan ke `/p/...` oleh shim.
- `history.pushState("/rute")` ditulis ulang ke `/p/https/host/rute`, `<base>` ikut disinkronkan, jadi relative fetch setelah navigasi klien tetap benar.

## PWA / offline

- `navigator.serviceWorker.register('/sw.js')` di-rewrite ke `/p/https/host/sw.js?__proxio_sw=1`; query penanda itu dibuang sebelum request ke situs, dan server menyuntik `swShimJS` ke script SW-nya.
- Di dalam worker, `fetch`, `Cache.prototype.{match,matchAll,add,addAll,put,delete}`, `CacheStorage.*`, dan `clients.openWindow` dipetakan ke URL proxy — jadi cache key dan precache Workbox memakai `/p/...` dan tetap cocok saat offline.
- `<base>` + shim halaman tetap dipakai untuk navigasi klien; `history.pushState` ikut disinkron.
- Kalau SW situs bikin kacau: buka `/__reset` (link di footer) — unregister semua SW + hapus semua cache.

Terverifikasi di Excalidraw (Workbox): SW `active` + mengontrol halaman, precache 16 entri + cache `fonts` 7 entri dengan key `http://localhost:8099/p/https/excalidraw.com/...`, `Page.getInstallabilityErrors` kosong (installable), dan **server dimatikan → reload tetap render penuh** dari cache SW.

## Mode forward proxy (CONNECT)

```bash
./proxio.exe -addr :8080 -forward :8081
```

`http://localhost:8081` adalah forward proxy biasa: browser yang melakukan TLS sendiri ke situs, tidak ada rewriting. Dipakai untuk situs yang tidak bisa hidup di balik proxy rewriting — **Cloudflare/CAPTCHA** dan **SSO** yang memeriksa fingerprint browser. Contoh pakai di Chrome (profil terpisah supaya browsing normal tidak ikut):

```bash
chrome.exe --user-data-dir=%TEMP%\proxio-fwd --proxy-server="http://localhost:8081"
```

Batasan mode ini: tidak ada rewrite (situs jalan apa adanya), tidak menyembunyikan IP, dan **bukan** alat melewati CAPTCHA — CAPTCHA tetap harus diselesaikan manusia, hanya saja sekarang browser-mu yang bicara langsung ke Cloudflare.

## Seamless routing: /proxy.pac

```bash
./proxio.exe -addr :8080 -forward :8081 -fwd-hosts "bps.go.id,cloudflare.com"
```

`http://localhost:8080/proxy.pac` mengembalikan PAC: host yang cocok → `PROXY 127.0.0.1:8081` (tunnel CONNECT, browser urus TLS sendiri), sisanya `DIRECT`. Set sekali di browser, tidak perlu flag atau ekstensi:

- Windows: Settings → Network & Internet → Proxy → **Use setup script** → `http://localhost:8080/proxy.pac` (Chrome/Edge ikut setelan sistem).
- Firefox: Settings → Network Settings → **Automatic proxy configuration URL** → URL yang sama.

Verified: `spkrt.web.bps.go.id`, `bps.go.id`, `www.cloudflare.com` → `PROXY 127.0.0.1:8081`; `example.com` → `DIRECT`. `-fwd-hosts "*"` menyalurkan semua lewat tunnel.

**Bukan** alat lewat CAPTCHA: Cloudflare memeriksa TLS/JA3 + UA + IP. Lewat tunnel, browser-mu yang bicara langsung, jadi challenge muncul dan diselesaikan seperti browsing biasa — dan `cf_clearance` yang sah tersimpan di profil itu.

## Docker

Port dan host tunnel dibaca dari `.env` (compose mensubstitusi `${VAR}`; default di file tetap jalan tanpa `.env`):

```bash
cp .env.example .env      # lalu ubah PORT_REWRITE / PORT_FORWARD / FWD_HOSTS
docker compose up -d --build
curl http://localhost:8080/__version
```

```dotenv
PORT_REWRITE=8080
PORT_FORWARD=8081
FWD_HOSTS=bps.go.id
```

Nilai yang sama dipakai untuk `ports` (host:container), flag `-addr`/`-forward`, dan healthcheck, jadi ganti port cukup di satu tempat. Port container sengaja dibuat sama dengan port host supaya `/proxy.pac` (yang mengembalikan `<host>:<PORT_FORWARD>`) tetap cocok saat diakses dari mesin lain. Env shell menang atas `.env`:

```bash
PORT_REWRITE=9090 docker compose up -d
```

`/proxy.pac` memakai host yang dipakai browser saat mengambilnya, jadi tetap benar kalau diakses dari mesin lain (`http://192.168.1.50:8080/proxy.pac` → `PROXY 192.168.1.50:8081`). Healthcheck memakai `wget` dari alpine ke `/__version`.

## Batasan yang diketahui

- **Cloudflare Turnstile / CF challenge tidak jalan di mode rewriting.** `api.js` mencari tag `<script>` miliknya sendiri berdasarkan URL asli; di mode rewriting URL-nya jadi `/p/https/challenges.cloudflare.com/...`, jadi muncul `Could not find Turnstile valid script tag` lalu widget gagal render. Pakai mode tunnel (`-forward` + `/proxy.pac`) untuk host seperti itu.
- Situs yang SW-nya menulis URL origin asli secara hardcode (`https://host/...` di luar `fetch`/`Cache`) tetap bisa meleset; shim hanya menangkap jalur yang lazim.
- Situs yang membandingkan `Response.url` / `location.origin` dengan origin asli bisa salah kira.
- `import()` modul dengan URL absolut, dan `<script>`/`<img>` lintas origin yang dibuat JS, dimuat langsung (fungsi jalan, tapi keluar dari proxy).
- Cookie berprefiks `__Host-` ditolak browser setelah `Secure` dibuang (butuh HTTPS).
- `srcset`, inline `<script>`, dan `<meta http-equiv=refresh>` tidak disentuh.
- Untuk publik: jalankan di belakang HTTPS (Caddy/nginx) + rate limit; proxy terbuka = open relay.

→ skipped: HTTPS lokal, auth/rate-limit. Add when dipakai publik.
