#!/usr/bin/env python3
"""Build reproducible, dependency-free hunt archives and SHA-256 checksums."""

import argparse
import gzip
import hashlib
import io
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import zipfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, help="release tag, e.g. v0.1.0")
    args = parser.parse_args()
    if not re.fullmatch(r"v\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?", args.version):
        parser.error("--version must be a release tag such as v0.1.0")
    root = Path(__file__).resolve().parent.parent
    destination = root / "dist"
    destination.mkdir(exist_ok=True)
    checksums = []
    targets = [(system, arch) for system in ("darwin", "linux", "windows") for arch in ("amd64", "arm64")]
    for system, arch in targets:
        binary_name = "hunt.exe" if system == "windows" else "hunt"
        with tempfile.TemporaryDirectory(prefix="hunt-release-") as temporary:
            binary = Path(temporary) / binary_name
            environment = os.environ.copy()
            environment.update(GOOS=system, GOARCH=arch, CGO_ENABLED="0")
            subprocess.run(
                ["go", "build", "-trimpath", "-buildvcs=false", "-ldflags",
                 f"-s -w -X main.version={args.version}", "-o", str(binary), "."],
                cwd=root, env=environment, check=True,
            )
            files = [(binary_name, binary.read_bytes(), 0o755)]
            for document in ("LICENSE", "README.md", "CHANGELOG.md", "COMPARISON.md",
                             "benchmark-results.txt", "live-benchmark.json",
                             "assets/hero.svg", "assets/terminal.svg"):
                files.append((document, (root / document).read_bytes(), 0o644))
            suffix = "zip" if system == "windows" else "tar.gz"
            archive = destination / f"hunt_{args.version}_{system}_{arch}.{suffix}"
            if suffix == "zip":
                with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as bundle:
                    for name, content, mode in files:
                        info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
                        info.external_attr = (0o100000 | mode) << 16
                        info.compress_type = zipfile.ZIP_DEFLATED
                        bundle.writestr(info, content)
            else:
                with archive.open("wb") as raw:
                    with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
                        with tarfile.open(fileobj=compressed, mode="w") as bundle:
                            for name, content, mode in files:
                                info = tarfile.TarInfo(name)
                                info.size, info.mode, info.mtime = len(content), mode, 0
                                bundle.addfile(info, io.BytesIO(content))
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()
            checksums.append(f"{digest}  {archive.name}")
            print(archive.name, flush=True)
    (destination / "SHA256SUMS").write_text("\n".join(checksums) + "\n")


if __name__ == "__main__":
    main()
