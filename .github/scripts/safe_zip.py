#!/usr/bin/env python3

import os
import stat
import zipfile
from pathlib import Path, PurePosixPath
from typing import NoReturn

ALLOWED_FLAGS = 0x0008 | 0x0800
ALLOWED_COMPRESSION = {zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED}


def fail(prefix: str, message: str) -> NoReturn:
    raise SystemExit(f"[{prefix}] {message}")


def parse_expected(
    values: list[str], *, maximum_total: int, prefix: str
) -> dict[str, int]:
    expected: dict[str, int] = {}
    for value in values:
        name, separator, maximum = value.rpartition(":")
        if not separator or not name or not maximum.isdecimal():
            fail(prefix, f"invalid expected member: {value}")
        limit = int(maximum)
        if (
            name in expected
            or "/" in name
            or "\\" in name
            or limit < 1
            or limit > maximum_total
        ):
            fail(prefix, f"invalid expected member: {value}")
        expected[name] = limit
    if not expected:
        fail(prefix, "at least one expected member is required")
    return expected


def safe_member_name(value: str, *, prefix: str) -> str:
    if not value or "\\" in value or "\x00" in value:
        fail(prefix, "archive contains an unsafe member name")
    path = PurePosixPath(value)
    if (
        path.is_absolute()
        or str(path) != value
        or any(part in ("", ".", "..") for part in path.parts)
    ):
        fail(prefix, "archive contains an unsafe member path")
    return path.name


def extract_exact_zip(
    archive_path: Path,
    output_path: Path,
    expected: dict[str, int],
    *,
    maximum_archive_bytes: int,
    maximum_total_output_bytes: int,
    block_size: int,
    prefix: str,
) -> None:
    if (
        not archive_path.is_file()
        or archive_path.stat().st_size < 1
        or archive_path.stat().st_size > maximum_archive_bytes
    ):
        fail(prefix, "archive input size is invalid")
    output_path.mkdir(mode=0o700, parents=True, exist_ok=False)

    try:
        with zipfile.ZipFile(archive_path, "r") as archive:
            infos = archive.infolist()
            if len(infos) != len(expected):
                fail(prefix, "archive member set differs")
            members: dict[str, zipfile.ZipInfo] = {}
            total_size = 0
            for info in infos:
                name = safe_member_name(info.filename, prefix=prefix)
                mode = info.external_attr >> 16
                if info.file_size > expected.get(name, 0) and name in expected:
                    fail(prefix, "archive member exceeds its output limit")
                if (
                    info.is_dir()
                    or name not in expected
                    or name in members
                    or info.flag_bits & ~ALLOWED_FLAGS
                    or info.compress_type not in ALLOWED_COMPRESSION
                    or (mode and stat.S_IFMT(mode) not in (0, stat.S_IFREG))
                    or info.file_size < 1
                    or info.compress_size < 0
                    or info.compress_size > maximum_archive_bytes
                ):
                    fail(prefix, f"archive member identity differs: {info.filename}")
                members[name] = info
                total_size += info.file_size
            if set(members) != set(expected) or total_size > maximum_total_output_bytes:
                fail(prefix, "archive member set or aggregate output size differs")

            for name, maximum in expected.items():
                info = members[name]
                destination = output_path / name
                with archive.open(info, "r") as source, destination.open("xb") as target:
                    remaining = maximum + 1
                    written = 0
                    while remaining:
                        block = source.read(min(block_size, remaining))
                        if not block:
                            break
                        target.write(block)
                        written += len(block)
                        remaining -= len(block)
                    if written != info.file_size or source.read(1):
                        fail(prefix, f"archive member output size differs: {info.filename}")
                os.chmod(destination, 0o600)
    except (OSError, ValueError, zipfile.BadZipFile, RuntimeError) as error:
        fail(prefix, f"archive is invalid: {error}")
