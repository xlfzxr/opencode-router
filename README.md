# OpenCode Claude-format bridge (Termux)

Bridge khusus **Anthropic Messages** untuk OpenCode v1. Ini bukan API Claude
resmi: model dijalankan oleh upstream OpenCode, bukan otomatis model Claude.
Tidak ada lagi endpoint publik Chat Completions/Responses atau supervisor
Node.js. Adapter wire internal masih diperlukan untuk model upstream yang
berbicara format Chat Completions/Responses.

```text
OpenCode v1 → POST /v1/messages → Go bridge
                                 ├─ katalog models.dev/api.json (via Tor)
                                 ├─ intersect katalog inference OpenCode
                                 ├─ urutkan model gratis + fallback/cooldown
                                 └─ inference opencode.ai (via Tor)
```

## Instalasi Termux

Jika sebelumnya memakai service Node dari versi lama, **jalankan `service stop`
sebelum memperbarui checkout**. Jangan jalankan `start-router` lama bersamaan.

Dari folder repo yang sudah diperbarui:

```sh
pkg install golang tor
cd ~/opencode-router
bash scripts/install-termux-service.sh
bash scripts/tor-up.sh
service start
service status
```

Installer membangun binary `ocr` dan memasang launcher `$PREFIX/bin/service`.
Node.js, npm, SQLite, root, dan systemd tidak dibutuhkan. Build pertama masih
mengunduh modul YAML. Command `service` milik program lain tidak ditimpa.

```sh
service start       # Go re-exec + setsid, background tanpa terminal
service stop        # SIGTERM + graceful shutdown, tunggu maksimal 35 detik
service restart
service status
service logs        # tail -n 100 -f; Ctrl+C hanya menutup penampil log
service foreground  # server langsung di terminal; Ctrl+C menghentikannya
```

Hentikan background dengan `service stop` sebelum memakai foreground pada port
yang sama. Tanpa launcher: `./ocr start -config ./configs/config.yaml` atau
`./ocr foreground -config ./configs/config.yaml`.

Start sukses berarti socket HTTP berhasil di-bind; **bukan** jaminan Tor sudah
bootstrap atau upstream dapat diakses. Error startup masuk ke log. PID memakai
identitas start-time `/proc` untuk mencegah salah menghentikan PID yang dipakai
ulang. Controller dikunci agar start/restart bersamaan tidak menduplikasi proses.

Tor dikelola terpisah lewat `scripts/tor-up.sh`, bukan dihentikan saat bridge
stop. Default proxy `socks5h://127.0.0.1:9050`, strict, tanpa fallback direct jika
proxy gagal. Jangan ubah konfigurasi proxy jika ingin seluruh koneksi bridge
melalui Tor. Endpoint hanya listen loopback secara default; tidak ada autentikasi
API lokal. Jangan mengeksposnya ke internet.

## OpenCode v1

Gabungkan bagian ini ke konfigurasi OpenCode kamu (jangan menimpa konfigurasi lain):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "ocr": {
      "npm": "@ai-sdk/anthropic",
      "name": "Local Claude-format bridge",
      "options": {
        "baseURL": "http://127.0.0.1:3002/v1",
        "apiKey": "local-bridge"
      },
      "models": {
        "ocr": {
          "name": "Auto free OpenCode model",
          "limit": { "context": 32768, "output": 4096 }
        }
      }
    }
  },
  "model": "ocr/ocr"
}
```

`local-bridge` hanya placeholder SDK, **bukan** API key Claude. Limit di contoh
adalah batas konservatif client, bukan jaminan kapasitas semua model. Sesuaikan
jika model yang dipilih memiliki limit lebih kecil.

Mendukung teks, gambar, tool use/result, tool choice, respons JSON, serta SSE
Anthropic (message/content-block start/delta/stop). Konten document/audio dan
server-side Anthropic tools belum didukung; konten yang tidak dikenal ditolak,
bukan diam-diam dihapus. Blok thinking/signature dari riwayat tidak diteruskan
karena tidak portabel antar-model. Bukan implementasi penuh API Anthropic
(count_tokens, prompt caching, extended thinking tidak dijanjikan).

## Katalog dan fallback

- `models.dev/api.json` adalah **metadata**, bukan endpoint inference.
- Hanya provider `opencode` dengan harga input **dan** output eksplisit `0` yang
  dipilih, lalu di-intersect dengan `/inference/v1/models` dari upstream.
- TTL katalog default 10 menit; refresh saat ada request setelah TTL habis.
  Tidak ada jaminan selalu paling baru atau bebas kuota/rate limit.
- Tidak ada daftar fallback model hardcoded. Jika katalog tidak tersedia atau
  tidak ada irisan, request gagal dengan error, bukan mencoba model berbayar.
- `routing.candidates: []` memakai semua model hasil discovery, diurutkan ID
  sebelum capability ordering/cooldown. Isi candidates untuk preferensi manual;
  model yang tidak ada di katalog tetap tidak dipakai.
- Alias `ocr` memakai fallback sesuai `max_attempts` (default 3). Model ID
  langsung hanya diterima jika ada di katalog gratis. Setelah SSE dikirim,
  bridge tidak berpindah model di tengah respons.

Endpoint publik:

- `POST /v1/messages`
- `GET /v1/models` (termasuk alias)
- `GET /health` atau `/healthz` (liveness, bukan kesiapan upstream)
- `GET /dashboard` dan `/v1/usage/stats`

`/v1/chat/completions` dan `/v1/responses` sudah dihapus (404).

## Data dan proses

- Statistik JSON: `~/.local/state/opencode-router/usage.json`; dapat diubah lewat
  `usage.path` atau `OCR_USAGE_PATH`. File SQLite lama tidak dimigrasikan/ditimpa.
- Snapshot ditulis atomik (temp + fsync + rename) setiap sekitar satu detik ketika
  berubah, di-flush saat shutdown, dengan advisory lock dan permission `0600`.
  Crash/force-kill dapat kehilangan statistik yang belum tersimpan. Antrean penuh
  menjatuhkan statistik agar tidak menghambat SSE.
- Retensi default 60 hari. Seluruh snapshot dimuat ke memori; untuk penggunaan
  pribadi, bukan jutaan request. Nilai `retention_days: 0` menyimpan semua data.
- Log dan state service: `~/.local/state/opencode-router/service/`; override
  `OCR_SERVICE_DIR` harus konsisten untuk semua command. Belum ada rotasi log.
- `setsid` tidak kebal Android task killer. Nonaktifkan optimasi baterai Termux;
  gunakan `termux-wake-lock` jika tersedia. Tidak ada auto-restart/auto-boot.
- Jika config shutdown lebih lama dari 35 detik, controller stop dapat timeout
  tanpa memaksa SIGKILL; periksa log/proses sebelum menjalankan ulang.

Setelah update kode, jalankan ulang installer lalu `service restart`.

## Tes

```sh
go test -race ./...
go vet ./...
```

Tes menggunakan upstream dan katalog dummy; tidak membutuhkan Tor atau inference
live. Pengujian kompatibilitas langsung di Android/OpenCode tetap diperlukan.

## Uji respons setiap model gratis dengan curl

Setelah Tor dan bridge berjalan:

```sh
pkg install curl jq
bash scripts/test-free-models.sh
```

Script mengambil ID aktual dari `/v1/models`, melewati alias `ocr`, lalu mengirim
request Messages non-streaming satu per satu. HTTP 200 dengan teks tidak kosong
berarti `ok`; respons kosong, error, dan timeout dicatat terpisah. Tes ini tidak
menguji streaming, tool use, atau akurasi jawaban. Karena memakai ID langsung,
fallback ke model lain tidak menyembunyikan kegagalan model yang diuji.

Output disimpan ke `~/opencode-model-tests/<timestamp>-<pid>/`: katalog, request,
respons, stderr curl, dan `results.jsonl`. Exit code 1 berarti ada probe gagal;
periksa error sebelum menyimpulkan model tidak tersedia (429/Tor/upstream bisa
bersifat sementara). Semua model diuji berurutan dengan jeda 2 detik dan timeout
90 detik per model agar tidak membanjiri upstream.

Override opsional:

```sh
OCR_PROBE_TIMEOUT=180 OCR_PROBE_DELAY=5 bash scripts/test-free-models.sh
```

Jika alias diubah di config, set `OCR_ALIAS` dengan nilai yang sama. Jika port
bridge diubah, gunakan `OCR_BASE_URL=http://127.0.0.1:PORT` (tanpa `/v1`).
