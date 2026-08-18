#!/usr/bin/env python3

import os
import stat
import sys
import zipfile
from pathlib import Path, PurePosixPath


MAX_ARCHIVE_BYTES = 4_000_000
MAX_TOTAL_OUTPUT_BYTES = 3_000_000
ALLOWED_FLAGS = 0x0008 | 0x0800  # data descriptor and UTF-8 names
ALLOWED_COMPRESSION = {zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED}


def fail(message: str) -> None:
    raise SystemExit(f"[actions-artifact] {message}")


def parse_expected(values: list[str]) -> dict[str, int]:
    expected: dict[str, int] = {}
    for value in values:
        name, separator, maximum = value.rpartition(":")
        if not separator or not name or not maximum.isdecimal():
            fail(f"invalid expected member: {value}")
        limit = int(maximum)
        if (
            name in expected
            or "/" in name
            or "\\" in name
            or limit < 1
            or limit > MAX_TOTAL_OUTPUT_BYTES
        ):
            fail(f"invalid expected member: {value}")
        expected[name] = limit
    if not expected:
        fail("at least one expected member is required")
    return expected


def safe_member_name(value: str) -> str:
    if not value or "\\" in value or "\x00" in value:
        fail("archive contains an unsafe member name")
    path = PurePosixPath(value)
    if (
        path.is_absolute()
        or str(path) != value
        or any(part in ("", ".", "..") for part in path.parts)
    ):
        fail("archive contains an unsafe member path")
    return path.name


def verify_info(info: zipfile.ZipInfo, expected: dict[str, int]) -> str:
    name = safe_member_name(info.filename)
    if info.is_dir() or name not in expected:
        fail(f"archive contains unexpected member: {info.filename}")
    if info.flag_bits & ~ALLOWED_FLAGS:
        fail(f"archive member has unsupported flags: {info.filename}")
    if info.compress_type not in ALLOWED_COMPRESSION:
        fail(f"archive member has unsupported compression: {info.filename}")
    mode = info.external_attr >> 16
    if mode and stat.S_IFMT(mode) not in (0, stat.S_IFREG):
        fail(f"archive member is not a regular file: {info.filename}")
    if info.file_size < 1 or info.file_size > expected[name]:
        fail(f"archive member exceeds its output limit: {info.filename}")
    if info.compress_size < 0 or info.compress_size > MAX_ARCHIVE_BYTES:
        fail(f"archive member has an invalid compressed size: {info.filename}")
    return name


def main() -> None:
    if len(sys.argv) < 4:
        fail(
            "usage: extract-verified-actions-artifact.py "
            "<archive.zip> <output-directory> <basename:max-bytes>..."
        )
    archive_path = Path(sys.argv[1])
    output_path = Path(sys.argv[2])
    expected = parse_expected(sys.argv[3:])
    if not archive_path.is_file() or archive_path.stat().st_size > MAX_ARCHIVE_BYTES:
        fail("archive exceeds its input limit")
    output_path.mkdir(mode=0o700, parents=True, exist_ok=False)

    try:
        with zipfile.ZipFile(archive_path, "r") as archive:
            infos = archive.infolist()
            if len(infos) != len(expected):
                fail("archive member set differs")
            members: dict[str, zipfile.ZipInfo] = {}
            total_size = 0
            for info in infos:
                name = verify_info(info, expected)
                if name in members:
                    fail(f"archive contains duplicate basename: {name}")
                members[name] = info
                total_size += info.file_size
            if set(members) != set(expected) or total_size > MAX_TOTAL_OUTPUT_BYTES:
                fail("archive member set or aggregate output size differs")

            for name, maximum in expected.items():
                info = members[name]
                destination = output_path / name
                with archive.open(info, "r") as source, destination.open("xb") as target:
                    remaining = maximum + 1
                    written = 0
                    while remaining:
                        block = source.read(min(65_536, remaining))
                        if not block:
                            break
                        target.write(block)
                        written += len(block)
                        remaining -= len(block)
                    if written != info.file_size or source.read(1):
                        fail(f"archive member output size differs: {info.filename}")
                os.chmod(destination, 0o600)
    except (OSError, ValueError, zipfile.BadZipFile, RuntimeError) as error:
        fail(f"archive is invalid: {error}")


if __name__ == "__main__":
    main()
