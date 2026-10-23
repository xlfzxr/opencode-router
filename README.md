# opencode-router

## Service background di Termux

Service memakai Node.js `child_process` untuk menjalankan binary Go sebagai
child dari supervisor background. Tidak membutuhkan systemd, root, atau npm
package tambahan.

```sh
pkg update
pkg install golang nodejs-lts
cd ~/opencode-router
bash scripts/install-termux-service.sh

service start
service status
service logs    # 100 baris terakhir + live; Ctrl+C hanya menutup penampil log
service stop
service restart
```

Installer membangun `./ocr` dan memasang command `$PREFIX/bin/service`. Jika
command `service` lain sudah ada, installer menolak menimpanya. Jalankan langsung
`node scripts/service.cjs start` (atau stop/status/restart/logs) bila ingin tetap
menggunakan service manager lain.

- Default konfigurasi: `configs/config.yaml`. Dashboard default:
  `http://127.0.0.1:3002/dashboard`.
- Command dapat digunakan dari direktori mana pun. Jangan memindahkan atau
  menghapus repo setelah instalasi; jalankan ulang installer jika dipindahkan.
- Log/PID: `~/.local/state/opencode-router/service/`. Log di-append, belum ada
  rotasi otomatis.
- `start` tidak menduplikasi proses. `stop` mengirim SIGTERM ke supervisor, yang
  meneruskannya ke router untuk graceful shutdown; setelah 30 detik child yang
  belum selesai akan diberi SIGKILL. Sesuaikan timeout ini di script jika
  `server.shutdown_timeout` disetel lebih lama dari 30 detik.
- Error startup langsung (misalnya config salah atau port terpakai) dilaporkan;
  status start berarti proses berhasil diluncurkan, **bukan** readiness HTTP atau
  kesiapan upstream/proxy. Periksa log untuk detail.
- Service tidak otomatis restart saat router crash, tidak otomatis hidup setelah
  reboot, dan tidak mengelola Tor. Tor opsional dapat dijalankan terpisah lewat
  `bash scripts/tor-up.sh`.
- Proses tetap berjalan setelah command selesai/terminal ditutup, tetapi Android
  tetap bisa membunuh Termux. Untuk penggunaan lama, jalankan `termux-wake-lock`
  (jika tersedia) dan nonaktifkan optimasi baterai untuk Termux. Wake lock tidak
  menjamin kebal terhadap Android task killer. Lepaskan dengan
  `termux-wake-unlock` ketika tidak diperlukan.

Override opsional (gunakan nilai yang sama untuk setiap command):

```sh
export OCR_CONFIG="$HOME/router-config.yaml"  # sebaiknya path absolut
export OCR_SERVICE_DIR="$HOME/.router-service"
# export OCR_BINARY="/absolute/path/to/ocr"
service start
```

Setelah update kode, jalankan ulang installer lalu `service restart` agar memakai
binary terbaru. Untuk uninstall, `service stop`, lalu hapus
`$PREFIX/bin/service` hanya jika itu launcher dari repo ini. Config, database
usage, dan log tidak dihapus oleh installer.

## Tes service controller

```sh
node --test scripts/service.test.cjs
```

Tes memakai child process dummy; tidak memerlukan Go, Termux, atau upstream.

## Storage statistik JSON (tanpa SQLite)

Statistik dashboard disimpan di
`~/.local/state/opencode-router/usage.json`. Build tidak lagi membutuhkan
`modernc.org/sqlite`, CGO, atau paket database tambahan. Modul YAML tetap perlu
tersedia/download pada build pertama. Penyimpanan JSON ditujukan untuk satu
instance router dan pemakaian pribadi, bukan riwayat besar jutaan request.

- `usage.path` di config memilih lokasi file JSON. Environment `OCR_USAGE_PATH`
  mengganti lokasi default; `OCR_USAGE_DB` lama tetap diterima sebagai fallback.
  Config `usage.path` yang terisi mengalahkan environment.
- File ditulis atomik dengan temporary file + fsync + rename, permission `0600`,
  dan exclusive advisory lock. Dua instance tidak boleh menulis path yang sama.
- Request masuk antrean nonblocking, disimpan maksimal setiap sekitar satu
  detik saat disk normal, dan di-flush saat graceful shutdown. Force-kill/crash
  dapat menghilangkan request yang belum tersimpan. Antrean penuh dapat
  menjatuhkan statistik, tetapi tidak menghambat respons router.
- `usage.retention_days` tetap berlaku (default 60 hari). Nilai `0` menyimpan
  seluruh riwayat. Snapshot dimuat ke memori dan ditulis ulang saat berubah;
  batasi retensi untuk menghemat memori/storage pada HP.
- File SQLite lama **tidak dimigrasikan, dihapus, atau ditimpa**. Default baru
  memakai `usage.json`, sehingga statistik mulai dari kosong. Bila config masih
  menunjuk file SQLite lama, ubah `usage.path` ke path JSON baru atau kosongkan
  agar memakai default. File rusak/format tidak didukung akan ditolak; router
  mencatat error dan menonaktifkan statistik untuk sesi itu, bukan merusak file.

Sesudah mengambil versi kode ini, build ulang dan restart:

```sh
cd ~/opencode-router
unset OCR_CONFIG OCR_SERVICE_DIR OCR_BINARY
bash scripts/install-termux-service.sh && service restart
```

Jangan jalankan launcher lama `start-router` bersama service baru. Jika launcher
lama sedang berjalan, hentikan dulu memakai `stop-router`.

Tes Go (termasuk persistensi JSON, retensi, lock, dan concurrency):

```sh
go test -race ./...
```
