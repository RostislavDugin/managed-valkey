#!/usr/bin/env python3
import argparse
import getpass
import os
import time
from pathlib import Path
from urllib.parse import urlsplit

from dotenv import load_dotenv
from valkey import Valkey

SECURE_SCHEMES = ("rediss", "valkeys")
SECURE_PORT = 41379
PLAIN_PORT = 6379


def env_flag(name):
    return os.environ.get(name, "").strip().lower() in ("1", "true", "yes", "on")


def parse_args():
    load_dotenv(Path(__file__).with_name(".env"))
    parser = argparse.ArgumentParser(
        description="Раз в секунду пишет значение в один адрес Valkey и читает его из другого.",
        epilog=(
            "Значения по умолчанию берутся из .env рядом со скриптом, образец в .env.example.\n"
            "Аргументы командной строки перекрывают .env.\n"
            "\n"
            "Примеры:\n"
            "  just run\n"
            "  just run 10.42.0.5:6379 10.42.0.6:6379\n"
            "  uv run python main.py rediss://cache.example.com:41379 rediss://cache-ro.example.com:41379"
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "write_address",
        nargs="?",
        default=os.environ.get("VALKEY_WRITE_ADDRESS"),
        help="адрес для записи: host:port или rediss://host:port",
    )
    parser.add_argument(
        "read_address",
        nargs="?",
        default=os.environ.get("VALKEY_READ_ADDRESS"),
        help="адрес для чтения: host:port или rediss://host:port",
    )
    parser.add_argument("password", nargs="?", default=os.environ.get("VALKEY_PASSWORD"))
    parser.add_argument("--user", default=os.environ.get("VALKEY_USER") or "app", help="пользователь ACL")
    parser.add_argument("--key", default=os.environ.get("VALKEY_KEY") or "manual-check", help="ключ для проверки")
    parser.add_argument("--interval", type=float, default=1.0, help="период в секундах")
    parser.add_argument("--timeout", type=float, default=1.0, help="таймаут подключения и команды")
    parser.add_argument(
        "--insecure",
        action="store_true",
        default=env_flag("VALKEY_INSECURE"),
        help="не проверять сертификат TLS",
    )
    args = parser.parse_args()
    if not args.write_address:
        parser.error("не задан адрес для записи: укажите аргументом или в VALKEY_WRITE_ADDRESS")
    if not args.read_address:
        parser.error("не задан адрес для чтения: укажите аргументом или в VALKEY_READ_ADDRESS")
    if not args.password:
        try:
            args.password = getpass.getpass("Пароль Valkey: ")
        except EOFError:
            parser.error("не задан пароль: укажите аргументом или в VALKEY_PASSWORD")
    return args


def build_client(address, args):
    parsed = urlsplit(address if "://" in address else f"//{address}")
    if not parsed.hostname:
        raise SystemExit(f"не разобрал адрес: {address}")
    secure = parsed.scheme in SECURE_SCHEMES
    return Valkey(
        host=parsed.hostname,
        port=parsed.port or (SECURE_PORT if secure else PLAIN_PORT),
        username=parsed.username or args.user,
        password=parsed.password or args.password,
        ssl=secure,
        ssl_cert_reqs=None if args.insecure else "required",
        ssl_check_hostname=secure and not args.insecure,
        socket_timeout=args.timeout,
        socket_connect_timeout=args.timeout,
        max_connections=1,
        decode_responses=True,
    )


def attempt(action):
    try:
        return action(), None
    except Exception as error:
        return None, " ".join(str(error).split()) or type(error).__name__


def check_read(value, expected, failure):
    if failure:
        return "FAIL", f"read: {failure}"
    if value is None:
        return "STALE", "read: значения нет"
    if expected is not None and value != expected:
        return "STALE", f"read: ждали {expected}, получили {value}"
    return "OK", None


def format_line(write_status, read_status, notes):
    line = f"{time.strftime('%H:%M:%S')}   write={write_status:<5} read={read_status:<5}"
    details = [note for note in notes if note]
    if details:
        line += "   " + "; ".join(details)
    return line.rstrip()


def format_totals(totals, ticks):
    parts = []
    for operation in ("write", "read"):
        counts = " ".join(f"{status}={count}" for status, count in sorted(totals[operation].items()))
        parts.append(f"{operation} {counts}")
    return f"итого: тиков {ticks}, " + ", ".join(parts)


def main():
    args = parse_args()
    writer = build_client(args.write_address, args)
    reader = build_client(args.read_address, args)

    totals = {"write": {}, "read": {}}
    ticks = 0
    written = None
    next_tick = time.monotonic()

    try:
        while True:
            ticks += 1
            candidate = str(ticks)

            _, write_failure = attempt(lambda: writer.set(args.key, candidate))
            if write_failure:
                write_status, write_note = "FAIL", f"write: {write_failure}"
            else:
                write_status, write_note = "OK", None
                written = candidate

            value, read_failure = attempt(lambda: reader.get(args.key))
            read_status, read_note = check_read(value, written, read_failure)

            totals["write"][write_status] = totals["write"].get(write_status, 0) + 1
            totals["read"][read_status] = totals["read"].get(read_status, 0) + 1
            print(format_line(write_status, read_status, (write_note, read_note)), flush=True)

            next_tick += args.interval
            delay = next_tick - time.monotonic()
            if delay > 0:
                time.sleep(delay)
            else:
                next_tick = time.monotonic()
    except KeyboardInterrupt:
        print()
        print(format_totals(totals, ticks), flush=True)


if __name__ == "__main__":
    main()
