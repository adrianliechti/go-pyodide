"""Regenerate reference PDFs using native pypdf + cryptography, never our adapter.

Use pypdf==6.19.0 and cryptography==50.0.1 in a temporary virtual environment.
This script is only needed to regenerate fixtures; Go tests run offline.
"""

from pathlib import Path

from pypdf import PdfReader, PdfWriter
from pypdf._crypt_providers import crypt_provider
from pypdf.generic import DecodedStreamObject, DictionaryObject, NameObject

assert crypt_provider[0] == "cryptography", crypt_provider
root = Path(__file__).resolve().parent
writer = PdfWriter()
page = writer.add_blank_page(width=300, height=200)
font = DictionaryObject({
    NameObject("/Type"): NameObject("/Font"),
    NameObject("/Subtype"): NameObject("/Type1"),
    NameObject("/BaseFont"): NameObject("/Helvetica"),
})
page[NameObject("/Resources")] = DictionaryObject({
    NameObject("/Font"): DictionaryObject({NameObject("/F1"): writer._add_object(font)}),
})
stream = DecodedStreamObject()
stream.set_data(b"BT /F1 12 Tf 30 100 Td (Hello encrypted PDF) Tj ET")
page[NameObject("/Contents")] = writer._add_object(stream.flate_encode())
writer.add_metadata({"/Title": "Crypto interoperability — Zürich"})
writer.write(root / "plain.pdf")

for algorithm in ("RC4-40", "RC4-128", "AES-128", "AES-256-R5", "AES-256"):
    writer = PdfWriter(clone_from=PdfReader(root / "plain.pdf"))
    writer.encrypt("user-password", "owner-password", algorithm=algorithm)
    writer.write(root / (algorithm.lower() + ".pdf"))

writer = PdfWriter(clone_from=PdfReader(root / "plain.pdf"))
writer.encrypt("", "owner-password", algorithm="AES-256")
writer.write(root / "aes-256-empty.pdf")
print("Generated reference PDFs with", crypt_provider)
