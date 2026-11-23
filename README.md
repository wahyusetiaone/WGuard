# WGuard

**WireGuard-over-TCP untuk VPS Muse** — klien WireGuard userspace yang jalan
di atas TCP, untuk VPS yang UDP-nya diblokir total dan tidak bisa membuat
perangkat TUN.

Dibangun dan diuji di VPS Muse pertama (Cloud A): peer `10.200.200.4`,
handshake jalan, panel WireGuard kebuka lewat terowongan, ADB ke HP tembus.

> **Satu Muse = satu peer.** Tiap Muse butuh `.conf` sendiri (private key +
> IP unik) dari panel WireGuard. Jangan pernah pakai ulang `.conf` Muse lain.

## Masalah yang diselesaikan

VPS Muse (container) tidak bisa jadi peer WireGuard biasa:

- `sendto`/`recvfrom` UDP **diblokir di level syscall**
  (`Operation not permitted`), bahkan ke localhost, bahkan di dalam user
  namespace.
- Perangkat TUN tidak bisa dibuat di namespace utama.
- Satu-satunya jalan keluar yang bebas: **TCP** (lewat SSH).

WGuard menjalankan WireGuard sepenuhnya di userspace, di atas TCP:

```
Muse VPS                          Cloud D (217.216.146.111)
┌──────────────────────┐          ┌───────────────────────────────────┐
│ wgtcp (Go, statis)   │          │ wg-tcp-relay.py (systemd)         │
│  - wireguard-go      │  TCP     │  TCP 127.0.0.1:51822              │
│  - Bind TCP khusus   │◄────────►│    ↕ framing 4-byte length        │
│  - netstack (TUN     │  via SSH │  UDP 127.0.0.1:51821 → wg0        │
│    userspace)        │  -L fwd  │    (server WireGuard asli)        │
│  - forward lokal:    │          │                                   │
│    15555 → HP :5555  │          │ wg0 peers:                        │
│    18088 → panel     │          │  .2 MikroTik  .3 HP  .4 Muse-A…  │
└──────────────────────┘          └───────────────────────────────────┘
```

## Isi repo

| File | Keterangan |
|---|---|
| `install.sh` | Installer: ambil binary (release / build dari source), pasang, tes SSH, nyalakan tunnel + klien, tunggu handshake. |
| `src/` | Source Go `wgtcp` (`main.go`, `go.mod`, `go.sum`) — untuk build dari source / audit. |
| `supervise.sh` | Watchdog lokal: nyalakan ulang tunnel/wgtcp bila mati. Dipakai cron 5 menit. |
| `wg-relay.py` | Relay TCP→UDP untuk Cloud D (**sudah terpasang sekali di Cloud D, dipakai bersama**). |
| `wg-tcp-relay.service` | Unit systemd relay Cloud D (**sudah terpasang sekali**). |
| `wg-muse.conf.example` | Template config — JANGAN dipakai apa adanya. |

Binary `wgtcp` yang sudah jadi (statis, x86-64) ada di
[Releases](https://github.com/wahyusetiaone/WGuard/releases) — installer
mengunduhnya dulu; bila gagal, ia build dari source secara otomatis.

## Prasyarat (sebelum install)

1. **Peer WireGuard sendiri di Cloud D.** Buat di panel WireGuard
   (`http://10.200.200.1:8088`, menu Tambah Device — akses via terowongan).
   Panel memberi `.conf` berisi private key + IP unik (mis. `10.200.200.5/32`).
   **Satu Muse = satu peer.** Dua klien dengan private key yang sama akan
   saling rebut sesi.
2. **Akses SSH key ke Cloud D sebagai root.** Public key Muse ini harus ada
   di `/root/.ssh/authorized_keys` Cloud D.
3. **Linux x86-64.** Tidak butuh root di VPS Muse (jalan sebagai user biasa);
   Go hanya dibutuhkan bila build dari source.

## Cara pakai (tutorial)

```bash
# 1. Salin repo ini
git clone https://github.com/wahyusetiaone/WGuard.git
cd WGuard

# 2. Siapkan .conf sendiri dari panel (langkah prasyarat no. 1),
#    simpan sebagai wg-muse.conf, lalu install:
./install.sh --config /path/to/wg-muse.conf
```

Installer memasang ke `~/wg-tcp/` (binary, `supervise.sh`, `wg-muse.conf`
chmod 600, folder `logs/`), mengetes SSH ke Cloud D, menyalakan terowongan
SSH + wgtcp, lalu menunggu handshake (maks ~30 detik). Sukses bila status
menunjukkan `last_handshake_time_sec` bukan 0.

Tanpa `--config`, installer memakai `wg-muse.conf` di folder repo bila ada.

## Verifikasi

```bash
# Handshake + trafik (tunggu last_handshake_time_sec != 0)
curl -s http://127.0.0.1:18080/status

# Panel WireGuard lewat terowongan (harus HTTP 302 = halaman login)
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:18088/

# ADB ke HP (hanya bila Wireless debugging NYALA di HP;
# bila mati, jawabannya "connection refused" — jalurnya sampai, portnya tutup)
adb connect 127.0.0.1:15555
```

Forward lokal yang disediakan wgtcp:

| Lokal | Tujuan | Keterangan |
|---|---|---|
| `127.0.0.1:15555` | `10.200.200.3:5555` | ADB HP (Wireless debugging) |
| `127.0.0.1:18088` | `10.200.200.1:8088` | Panel WireGuard |
| `127.0.0.1:18080/status` | — | Status WG (tanpa private key) |

## Watchdog (cron 5 menit, di VPS Muse)

Supervise script sudah ikut terpasang (`~/wg-tcp/supervise.sh`). Supaya
tunnel hidup lagi sesudah VM diganti / proses mati, pasang cron interval
5 menit di Muse:

- **id:** `wg-tunnel-watchdog`
- **jadwal:** interval, tiap `5m`
- **isi tugas:** jalankan `~/wg-tcp/supervise.sh`, lalu baca 10 baris
  terakhir `~/wg-tcp/logs/supervise.log`. Bila tidak ada baris baru soal
  restart/kegagalan, diam (jangan kirim apa-apa). Bila ada komponen yang
  di-restart atau gagal start, kirim pesan singkat ke chat utama
  (komponen apa, jam berapa, apakah sekarang sudah hidup lagi).

## Relay Cloud D (SUDAH terpasang — hanya bila perlu pasang ulang)

Relay ini **satu untuk semua Muse**. Jangan install per Muse.

```bash
# di Cloud D sebagai root:
cp wg-relay.py /usr/local/bin/wg-relay.py
cp wg-tcp-relay.service /etc/systemd/system/wg-tcp-relay.service
systemctl daemon-reload
systemctl enable --now wg-tcp-relay.service
systemctl status wg-tcp-relay.service
ss -ltn | grep 51822   # harus LISTEN 127.0.0.1:51822
```

Cek log relay: `journalctl -u wg-tcp-relay.service -f`

## Troubleshooting

| Gejala | Kemungkinan penyebab |
|---|---|
| `tcpBind: not connected` berulang | Terowongan SSH mati — cek `ss -ltn \| grep 51822`; jalankan `supervise.sh`. |
| Handshake tidak selesai, `tx_bytes=0` | Relay Cloud D mati — cek `systemctl status wg-tcp-relay.service` di Cloud D. |
| `Failed to send handshake initiation` di awal lalu sembuh | Normal: peer sempat kirim sebelum TCP tersambung; retry ke-2 berhasil. |
| `adb connect` → `connection refused` | Jalur sampai, tapi Wireless debugging di HP belum nyala. |
| `adb connect` → timeout | Tunnel/peer belum handshake — cek `/status` dan log wgtcp. |
| Dua Muse pakai `.conf` yang sama | Jangan. Buat peer baru di panel untuk Muse kedua. |
| `install.sh` gagal SSH | Public key Muse ini belum ada di `/root/.ssh/authorized_keys` Cloud D. |
| `install.sh` gagal unduh binary + gagal build | Cek akses internet ke github.com / go.dev dari VPS. |

## Keamanan

- Private key tiap peer **hanya ada di `.conf` Muse itu** (`wg-muse.conf`,
  chmod 600). Jangan taruh di repo, jangan kirim lewat chat umum.
- Public key server (`3oC/jW09…`) boleh disebar — itu kunci publik.
- Relay di Cloud D hanya listen di `127.0.0.1:51822` (bukan publik);
  yang bisa nyambung hanya lewat SSH tunnel.
- Repo ini **tidak berisi kredensial apa pun** — config contoh hanya
  placeholder.

## Build dari source (manual)

```bash
# butuh Go 1.22+; modul diambil dari proxy Go (butuh akses internet)
cd src
go mod tidy
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o wgtcp .
```

Catatan: `gvisor.dev/gvisor` harus versi yang dipin wireguard-go
(`v0.0.0-20250503011706-39ed1f5ac29c`) — versi terbaru saat penulisan
paket ini rusak (file `bridge_test.go` nyasar ke package `stack`).

Juga perlu diingat dari pembangunan awal: `Bind.Close()` dipanggil
`wireguard-go` SEBELUM `Open()` (saat `IpcSet`) — stop channel harus
per-generasi, kalau tidak reader mati diam-diam dan handshake gagal
dengan pesan `not connected`.
