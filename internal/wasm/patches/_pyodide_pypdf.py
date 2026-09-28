"""pypdf adapter for the host AES primitives; loaded only when pypdf is used."""

import secrets

from _pyodide_crypto import (
    _key_bytes,
    aes_cbc_decrypt,
    aes_cbc_encrypt,
    aes_ecb_decrypt,
    aes_ecb_encrypt,
)
from pypdf._crypt_providers._base import CryptBase
from pypdf._utils import logger_warning
from pypdf.errors import PdfStreamError


def _pad(data):
    count = 16 - len(data) % 16
    return data + bytes([count]) * count


class CryptAES(CryptBase):
    def __init__(self, key):
        self.key = _key_bytes(key)

    def encrypt(self, data):
        iv = secrets.token_bytes(16)
        return iv + aes_cbc_encrypt(self.key, iv, _pad(data))

    def decrypt(self, data, *, strict=True):
        # pypdf accepts an empty encrypted stream, including an IV alone.
        payload = data[16:]
        if not payload:
            return b""
        if len(payload) % 16 and not strict:
            logger_warning("Adding missing padding.", source=__name__)
            payload = _pad(payload)
        try:
            plain = aes_cbc_decrypt(self.key, data[:16], payload)
        except ValueError as error:
            raise PdfStreamError(str(error)) from error

        count = plain[-1]
        if not 1 <= count <= 16 or plain[-count:] != bytes([count]) * count:
            if strict:
                raise PdfStreamError("Invalid PKCS#7 padding")
            # Preserve pypdf's lenient handling of malformed PDF streams.
            logger_warning("Ignoring invalid AES padding.", source=__name__)
        return plain[:-count]


def install(provider):
    # Keep any working backend supplied by the application. Only extend the
    # pure-Python fallback, retaining pypdf's existing RC4 implementation.
    if provider.crypt_provider[0] != "local_crypt_fallback":
        return
    for name in ("CryptAES", "aes_cbc_decrypt", "aes_cbc_encrypt", "aes_ecb_decrypt", "aes_ecb_encrypt"):
        setattr(provider, name, globals()[name])
    provider.crypt_provider = ("go-pyodide", "1")
