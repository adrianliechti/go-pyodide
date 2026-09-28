"""Run inside WASI. Check native-backend fixtures and new encrypted documents."""

from io import BytesIO
import os
from pathlib import Path

from pypdf import PdfReader, PdfWriter
from pypdf._crypt_providers import CryptAES, aes_cbc_encrypt, crypt_provider
from pypdf.errors import PdfStreamError

assert crypt_provider == ("go-pyodide", "1"), crypt_provider
root = Path("/fixtures")
title = "Crypto interoperability — Zürich"


def check_document(reader):
    assert len(reader.pages) == 1
    assert reader.pages[0].extract_text() == "Hello encrypted PDF"
    assert reader.metadata.title == title


def check_passwords(data, user="user-password", owner="owner-password"):
    for password, result in (("wrong-password", 0), (user, 1), (owner, 2)):
        reader = PdfReader(BytesIO(data), strict=True)
        assert reader.is_encrypted
        assert reader.decrypt(password) == result, (password, result)
        if result:
            check_document(reader)
            plain = BytesIO()
            PdfWriter(clone_from=reader).write(plain)
            decoded = PdfReader(BytesIO(plain.getvalue()), strict=True)
            assert not decoded.is_encrypted
            check_document(decoded)


for algorithm in ("RC4-40", "RC4-128", "AES-128", "AES-256-R5", "AES-256"):
    check_passwords((root / (algorithm.lower() + ".pdf")).read_bytes())
    writer = PdfWriter(clone_from=PdfReader(root / "plain.pdf"))
    writer.encrypt("user-password", "owner-password", algorithm=algorithm)
    stream = BytesIO()
    writer.write(stream)
    check_passwords(stream.getvalue())
    if output := os.environ.get("PDF_OUTPUT_DIR"):
        (Path(output) / (algorithm.lower() + ".pdf")).write_bytes(stream.getvalue())

check_passwords((root / "aes-256-empty.pdf").read_bytes(), user="")
check_document(PdfReader(root / "aes-256-empty.pdf"))

# AES-256's password normalization and hashing also run inside WASI.
writer = PdfWriter(clone_from=PdfReader(root / "plain.pdf"))
writer.encrypt("pässword-東京", "owner-秘密", algorithm="AES-256")
stream = BytesIO()
writer.write(stream)
check_passwords(stream.getvalue(), user="pässword-東京", owner="owner-秘密")

# Padding at block and host-chunk boundaries, fresh IVs and malformed input.
for size in (0, 1, 15, 16, 17, 32767, 32768, 32769, 65537):
    cipher = CryptAES(b"k" * 32)
    data = b"x" * size
    encrypted = cipher.encrypt(data)
    assert len(encrypted) == 16 + (size // 16 + 1) * 16
    assert cipher.decrypt(encrypted) == data
    assert cipher.encrypt(data) != encrypted
assert cipher.decrypt(b"") == cipher.decrypt(b"\0" * 16) == b""

iv = b"i" * 16
bad_padding = iv + aes_cbc_encrypt(b"k" * 32, iv, b"x" * 15 + b"\0")
for damaged in (bad_padding, iv + b"truncated"):
    try:
        cipher.decrypt(damaged)
    except PdfStreamError:
        pass
    else:
        raise AssertionError("malformed encrypted stream accepted in strict mode")
assert cipher.decrypt(bad_padding, strict=False) == b""
assert isinstance(cipher.decrypt(iv + b"truncated", strict=False), bytes)

print("encrypted PDFs ok")
