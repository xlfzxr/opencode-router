# torctl

Supervisor tunggal untuk Tor + Snowflake + HTTP bridge — **satu binary Go,
satu perintah**. Tidak ada shell script, tidak ada `pkill` manual.

```
torctl start    → nyalakan snowflake, nyalakan Tor (child), tunggu bootstrap
                  100%, nyalakan HTTP bridge (foreground)
torctl stop     → matikan bridge → Tor → snowflake, bersihkan state
torctl status   → PID, port, IP keluar Tor
torctl logs     → tail log tor + bridge + snowflake
torctl restart  → stop + start
```

Semua proses dikelola sebagai **child process dari torctl** (bukan daemon
terpisah), sehingga:

* kalau `torctl` mati → semua ikut mati (rapi, no orphan) — setiap child
  dibunuh otomatis lewat `Pdeathsig`, plus deteksi anak yatim saat `start`;
* state disimpan di `$PREFIX/var/run/torctl/`;
* log disimpan di `$PREFIX/var/log/torctl/`.

Di Termux, `PREFIX=/data/data/com.termux/files/usr` sehingga lokasinya:

```
$PREFIX/var/run/torctl/    torctl.pid, tor.pid, snowflake.pid, torctl.torrc
$PREFIX/var/log/torctl/    tor.log, bridge.log, snowflake.log
$PREFIX/var/lib/torctl/    DataDirectory tor (bertahan antar restart)
```

Tanpa `PREFIX` (Linux desktop), prefix jatuh ke `~/.torctl/` dengan layout
`run/`, `log/`, `lib/`.

## Build

```bash
go build -o torctl .
# Termux: pkg install golang && go build -o torctl .
# tanpa flag -ldflags apa pun — tidak ada dependensi/linkname khusus
```

Tidak ada dependensi eksternal — `go.mod` murni stdlib (dialer SOCKS5
ditulis sendiri di `bridge.go`).

## Pakai

```bash
./torctl start          # sesi 1: biarkan jalan
                        # → tor started (pid ...), waiting bootstrap...
                        # → tor bootstrapped 100%
                        # → bridge listening on 127.0.0.1:8118 -> 127.0.0.1:9050

./torctl status         # sesi 2
./torctl logs           # tail tor.log + bridge.log + snowflake.log
./torctl logs -f -n 50  # follow, 50 baris
curl -x http://127.0.0.1:8118 -s https://check.torproject.org/api/ip
./torctl stop           # bridge → tor → snowflake, state dibersihkan
```

## Desain

* **tor** dijalankan child process (`Setpgid` + `Pdeathsig=SIGKILL`),
  stdout/stderr diarahkan ke `tor.log`; start menunggu
  `Bootstrapped 100%` (maks. `TORCTL_BOOTSTRAP_TIMEOUT`, default 180s).
* **snowflake** juga child process torctl — dijalankan dengan environment
  protokol Managed Transport (`TOR_PT_MANAGED_TRANSPORT_VER=1`,
  `TOR_PT_CLIENT_TRANSPORTS=snowflake`), lalu alamat SOCKS lokalnya dibaca
  dari baris `CMETHOD snowflake SOCKS5 127.0.0.1:PORT` di stdout.
  Tor memakai bentuk **eksternal**:

  ```
  UseBridges 1
  ClientTransportPlugin snowflake socks5 127.0.0.1:PORT
  Bridge snowflake 192.0.2.3:1 2 B8DA951F1B4652FB6945D394F2D6487AB75B2668
  ```

  (dua bentuk `ClientTransportPlugin` — `exec` dan `socks4|socks5` —
  didokumentasikan resmi di manpage tor; torctl memakai `socks5` supaya
  snowflake tetap child torctl, bukan child tor.)
* **bridge HTTP ↔ SOCKS5** berjalan di dalam proses torctl (by design):
  `CONNECT` (HTTPS) dan plain-HTTP dua-duanya dial lewat SOCKS5 tor.
  `torctl stop` mengirim SIGTERM ke supervisor; supervisor menutup bridge
  lebih dulu, baru mematikan tor, lalu snowflake.
* **torrc digenerate ulang** di `run/torctl.torrc` setiap start — torrc
  milik sistem/tidak disentuh.

## Konfigurasi (environment)

| Variabel | Default | Keterangan |
|---|---|---|
| `TORCTL_SNOWFLAKE` | `1` | `0` = matikan snowflake (plain Tor / bridge custom) |
| `TORCTL_BRIDGES` | line snowflake bawaan | bridge custom, dipisah `;` atau newline; hanya transport `snowflake` (atau `ip:port` polos) yang dikelola torctl |
| `TORCTL_SOCKS_ADDR` | `127.0.0.1:9050` | SOCKS5 tor |
| `TORCTL_BRIDGE_ADDR` | `127.0.0.1:8118` | HTTP bridge |
| `TORCTL_TOR_BIN` | `tor` | binary tor |
| `TORCTL_SNOWFLAKE_BIN` | `snowflake-client` | binary snowflake |
| `TORCTL_SNOWFLAKE_URL` | broker resmi Tor Project | URL broker |
| `TORCTL_SNOWFLAKE_FRONTS` | (kosong) | front domain (dipisah koma) untuk domain fronting |
| `TORCTL_SNOWFLAKE_ICE` | STUN list | daftar STUN |
| `TORCTL_BOOTSTRAP_TIMEOUT` | `180s` | batas tunggu bootstrap |
| `TORCTL_EXTRA_TORRC` | — | path torrc tambahan (`Include`) |
| `TORCTL_PREFIX` / `PREFIX` | Termux: `$PREFIX`, selain itu `~/.torctl` | prefix state/log |
| `TORCTL_CHECK_URL` | `https://check.torproject.org/api/ip` | dipakai `status` |

Contoh:

```bash
# plain Tor tanpa snowflake (mis. jaringan terbuka)
TORCTL_SNOWFLAKE=0 ./torctl start

# snowflake dengan bridge custom
TORCTL_BRIDGES="snowflake 192.0.2.3:1 2 <FINGERPRINT>" ./torctl start
```

## Persyaratan

* Linux (Termux/Android punya kok) — memakai `Pdeathsig`/`Setpgid`.
* `tor` di `PATH` (Termux: `pkg install tor`).
* `snowflake-client` di `PATH` (Termux: `pkg install snowflake-client`)
  — atau set `TORCTL_SNOWFLAKE=0`.

## Catatan

* `start` berjalan **foreground** (desain: satu sesi = satu supervisor).
  `stop`/`status`/`logs` dari sesi lain bebas.
* Bridge in-process: PID bridge = PID torctl (lihat `status`: baris
  `bridge ... (in-process torctl)`). Kalau suatu saat butuh bridge sebagai
  child terpisah, tinggal pindahkan goroutine accept ke `launch()`.
* Urutan shutdown: **bridge → tor → snowflake → state** (sesuai desain).
