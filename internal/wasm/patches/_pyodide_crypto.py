"""Internal AES primitives backed by Go's crypto/aes and crypto/cipher."""

import os


def _key_bytes(key):
    key = bytes(memoryview(key))
    if len(key) not in (16, 24, 32):
        raise ValueError("AES keys must contain 16, 24 or 32 bytes")
    return key


def _write(fd, data):
    view = memoryview(data)
    while view:
        count = os.write(fd, view)
        if count == 0:
            raise OSError("AES device stopped accepting input")
        view = view[count:]


def _aes(mode, operation, key, iv, data):
    key = _key_bytes(key)
    iv = bytes(memoryview(iv))
    data = memoryview(data).cast("B")
    if mode == "cbc" and len(iv) != 16:
        raise ValueError("AES-CBC IV must contain 16 bytes")
    if len(data) % 16:
        raise ValueError("AES data length must be a multiple of 16")

    fd = os.open(f"/dev/pyodide/crypto/aes/{mode}/{operation}/{len(key)}", os.O_RDWR)
    try:
        _write(fd, key + iv)
        os.fdatasync(fd)
        result = bytearray()
        for offset in range(0, len(data), 32768):
            chunk = data[offset:offset + 32768]
            _write(fd, chunk)
            remaining = len(chunk)
            while remaining:
                block = os.read(fd, remaining)
                if not block:
                    raise OSError("AES device returned incomplete output")
                result.extend(block)
                remaining -= len(block)
        os.fsync(fd)
        return bytes(result)
    finally:
        os.close(fd)


def aes_ecb_encrypt(key, data):
    return _aes("ecb", "encrypt", key, b"", data)


def aes_ecb_decrypt(key, data):
    return _aes("ecb", "decrypt", key, b"", data)


def aes_cbc_encrypt(key, iv, data):
    return _aes("cbc", "encrypt", key, iv, data)


def aes_cbc_decrypt(key, iv, data):
    return _aes("cbc", "decrypt", key, iv, data)
