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
