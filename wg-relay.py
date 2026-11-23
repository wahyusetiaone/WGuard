#!/usr/bin/env python3
"""wg-relay.py — carry WireGuard UDP datagrams across a TCP/UNIX stream.

WireGuard speaks UDP only, but this VM cannot send UDP to the internet.
The relay chain is:

  boringtun (inside netns, UDP 127.0.0.1:51821)
    <-> relay-A  udp2stream  (inside netns, UDP <-> UNIX stream)
    <-> relay-B  stream2stream (host, UNIX <-> TCP via SSH -L)
    <-> relay-C  stream2udp  (Cloud D, TCP <-> UDP 127.0.0.1:51821 = wg0)

Stream framing: 4-byte big-endian length + payload. One datagram = one frame.
stream2udp serves MULTIPLE clients at once: each TCP connection gets its own
thread and its own UDP socket, so replies from wg0 can only belong to the
client that owns that socket (no demultiplexing needed). udp2stream is still
single-peer (replies go to the last-seen sender).
On any stream error the bridge for that client ends; the relay keeps serving
the others. The process exits non-zero only on a fatal listen/bind error.
"""
import argparse
import socket
import struct
import sys
import threading
import time

LOG_PREFIX = "wg-relay"


def log(msg):
    print(f"{time.strftime('%H:%M:%S')} [{LOG_PREFIX}] {msg}", flush=True)


def parse_addr(spec):
    """unix:/path | tcp:host:port | tcp-listen:host:port | udp:host:port"""
    kind, rest = spec.split(":", 1)
    if kind == "unix":
        return ("unix", rest)
    if kind in ("tcp", "tcp-listen"):
        host, port = rest.rsplit(":", 1)
        return (kind, host, int(port))
    if kind == "udp":
        host, port = rest.rsplit(":", 1)
        return (kind, host, int(port))
    raise ValueError(f"bad address spec: {spec}")


def recvall(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("stream closed")
        buf += chunk
    return buf


def stream_reader(sock, on_frame, stop):
    """Read length-prefixed frames from a stream socket."""
    try:
        while not stop.is_set():
            hdr = recvall(sock, 4)
            (ln,) = struct.unpack(">I", hdr)
            if ln <= 0 or ln > 65535:
                raise ValueError(f"bad frame length {ln}")
            payload = recvall(sock, ln)
            on_frame(payload)
    except (ConnectionError, OSError, ValueError) as e:
        log(f"stream reader ended: {e}")
    finally:
        stop.set()


def stream_writer(sock, queue, stop):
    """Write length-prefixed frames to a stream socket (queue fed by threads)."""
    import queue as q

    try:
        while not stop.is_set():
            try:
                payload = queue.get(timeout=0.5)
            except q.Empty:
                continue
            sock.sendall(struct.pack(">I", len(payload)) + payload)
    except (ConnectionError, OSError) as e:
        log(f"stream writer ended: {e}")
    finally:
        stop.set()


def udp_loop(udp_sock, target, to_stream_q, stop, is_server_side):
    """Read datagrams from udp_sock, forward to stream queue.
    Replies from the stream go to the last-seen sender (server side) or to
    the fixed target (client side). Returns a send function for stream->udp."""
    last_sender = {"addr": target if not is_server_side else None}

    def send_to_udp(payload):
        addr = last_sender["addr"]
        if addr is None:
            return
        try:
            udp_sock.sendto(payload, addr)
        except OSError as e:
            log(f"udp send failed: {e}")

    def pump():
        udp_sock.settimeout(0.5)
        while not stop.is_set():
            try:
                data, addr = udp_sock.recvfrom(65535)
            except socket.timeout:
                continue
            except OSError:
                if stop.is_set():
                    return
                continue
            if is_server_side:
                last_sender["addr"] = addr
            to_stream_q.put(data)

    t = threading.Thread(target=pump, daemon=True)
    t.start()
    return send_to_udp


def run_udp2stream(udp_spec, stream_spec):
    _, host, port = parse_addr(udp_spec)
    udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    udp.bind((host, port))
    log(f"UDP listening on {host}:{port}")

    kind = parse_addr(stream_spec)[0]
    assert kind == "unix", "udp2stream expects a unix stream listener"
    path = parse_addr(stream_spec)[1]
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        import os

        os.unlink(path)
    except FileNotFoundError:
        pass
    srv.bind(path)
    srv.listen(1)
    log(f"UNIX listening on {path}")
    while True:
        conn, _ = srv.accept()
        log("stream peer connected")
        run_bridge(udp_sock=udp, udp_target=None, is_server_side=True,
                    stream_sock=conn)
        try:
            conn.close()
        except OSError:
            pass
        log("waiting for next stream peer")


def run_stream2stream(a_spec, b_spec):
    import queue as q

    while True:
        stop = threading.Event()
        try:
            # side A: unix client
            _, a_path = parse_addr(a_spec)
            # side B: tcp client
            _, b_host, b_port = parse_addr(b_spec)
            qa, qb = q.Queue(), q.Queue()
            sa = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            log(f"connecting UNIX {a_path}")
            sa.connect(a_path)
            sb = socket.create_connection((b_host, b_port))
            log(f"connected TCP {b_host}:{b_port}")
        except OSError as e:
            log(f"connect failed ({e}), retrying in 2s")
            time.sleep(2)
            continue
        t1 = threading.Thread(target=stream_reader, args=(sa, qb.put, stop), daemon=True)
        t2 = threading.Thread(target=stream_writer, args=(sb, qb, stop), daemon=True)
        t3 = threading.Thread(target=stream_reader, args=(sb, qa.put, stop), daemon=True)
        t4 = threading.Thread(target=stream_writer, args=(sa, qa, stop), daemon=True)
        for t in (t1, t2, t3, t4):
            t.start()
        while not stop.is_set():
            time.sleep(0.2)
        log("stream2stream bridge ended, reconnecting in 1s")
        for s in (sa, sb):
            try:
                s.close()
            except OSError:
                pass
        time.sleep(1)


def serve_stream2udp_client(conn, addr, uhost, uport):
    """Serve one TCP client: own UDP socket so wg0 replies route back here."""
    udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    udp.bind(("127.0.0.1", 0))
    try:
        run_bridge(udp_sock=udp, udp_target=(uhost, uport), is_server_side=False,
                   stream_sock=conn)
    finally:
        try:
            conn.close()
        except OSError:
            pass
        try:
            udp.close()
        except OSError:
            pass
        log(f"stream peer {addr} disconnected")


def run_stream2udp(stream_spec, udp_spec):
    _, host, port = parse_addr(stream_spec)
    assert parse_addr(stream_spec)[0] == "tcp-listen"
    _, uhost, uport = parse_addr(udp_spec)
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((host, port))
    srv.listen(16)
    log(f"TCP listening on {host}:{port} (multi-client)")
    while True:
        conn, addr = srv.accept()
        log(f"stream peer connected from {addr}")
        t = threading.Thread(target=serve_stream2udp_client,
                             args=(conn, addr, uhost, uport), daemon=True)
        t.start()


def run_bridge(udp_sock, udp_target, is_server_side, stream_sock):
    import queue as q

    stop = threading.Event()
    to_stream = q.Queue()
    send_to_udp = udp_loop(udp_sock, udp_target, to_stream, stop, is_server_side)
    t1 = threading.Thread(target=stream_reader, args=(stream_sock, send_to_udp, stop), daemon=True)
    t2 = threading.Thread(target=stream_writer, args=(stream_sock, to_stream, stop), daemon=True)
    t1.start()
    t2.start()
    while not stop.is_set():
        time.sleep(0.2)
    log("bridge exiting")
    return 1


def main():
    ap = argparse.ArgumentParser(description="WireGuard UDP-over-stream relay")
    sub = ap.add_subparsers(dest="mode", required=True)
    p = sub.add_parser("udp2stream")
    p.add_argument("--udp", required=True, help="udp:host:port to listen on")
    p.add_argument("--stream", required=True, help="unix:/path to listen on")
    p = sub.add_parser("stream2stream")
    p.add_argument("--a", required=True, help="unix:/path (client)")
    p.add_argument("--b", required=True, help="tcp:host:port (client)")
    p = sub.add_parser("stream2udp")
    p.add_argument("--stream", required=True, help="tcp-listen:host:port")
    p.add_argument("--udp", required=True, help="udp:host:port target")
    args = ap.parse_args()

    if args.mode == "udp2stream":
        return run_udp2stream(args.udp, args.stream)
    if args.mode == "stream2stream":
        return run_stream2stream(args.a, args.b)
    if args.mode == "stream2udp":
        return run_stream2udp(args.stream, args.udp)
    return 2


if __name__ == "__main__":
    sys.exit(main())
