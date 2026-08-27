#!/usr/bin/env python3

import sys
from pathlib import Path

from safe_zip import extract_exact_zip, fail, parse_expected

PREFIX = "staged-release-artifact"
MAX_ARCHIVE_BYTES = 2_000_000_000
MAX_TOTAL_OUTPUT_BYTES = 2_000_000_000


def main() -> None:
    if len(sys.argv) < 4:
        fail(
            PREFIX,
            "usage: extract-staged-release-artifact.py "
            "<archive.zip> <output-directory> <basename:max-bytes>...",
        )
    expected = parse_expected(
        sys.argv[3:], maximum_total=MAX_TOTAL_OUTPUT_BYTES, prefix=PREFIX
    )
    extract_exact_zip(
        Path(sys.argv[1]),
        Path(sys.argv[2]),
        expected,
        maximum_archive_bytes=MAX_ARCHIVE_BYTES,
        maximum_total_output_bytes=MAX_TOTAL_OUTPUT_BYTES,
        block_size=1_048_576,
        prefix=PREFIX,
    )


if __name__ == "__main__":
    main()
