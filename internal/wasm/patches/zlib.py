"""zlib for the WASI build, backed by the Go host.

The CPython WASI build ships without zlib. This module keeps the standard
zlib API and delegates the work to the host through device files under
/dev/pyodide/zlib (see zlib.go in the Go package): writes feed data, reads
drain output, fdatasync flushes and fsync finishes a stream, ftruncate sets
the output budget of a decompressor, and pread returns a status record whose
payload hands back the input the host has not consumed.
"""

import os
import binascii

__all__ = [
    "compress", "decompress", "compressobj", "decompressobj", "crc32",
    "adler32", "error", "MAX_WBITS", "DEFLATED", "DEF_MEM_LEVEL",
    "DEF_BUF_SIZE", "Z_NO_COMPRESSION", "Z_BEST_SPEED", "Z_BEST_COMPRESSION",
    "Z_DEFAULT_COMPRESSION", "Z_FILTERED", "Z_HUFFMAN_ONLY", "Z_RLE",
    "Z_FIXED", "Z_DEFAULT_STRATEGY", "Z_NO_FLUSH", "Z_PARTIAL_FLUSH",
    "Z_SYNC_FLUSH", "Z_FULL_FLUSH", "Z_FINISH", "Z_BLOCK", "Z_TREES",
    "ZLIB_VERSION", "ZLIB_RUNTIME_VERSION",
]

_DEV = "/dev/pyodide/zlib"

MAX_WBITS = 15
DEFLATED = 8
DEF_MEM_LEVEL = 8
DEF_BUF_SIZE = 16384
Z_NO_COMPRESSION = 0
Z_BEST_SPEED = 1
Z_BEST_COMPRESSION = 9
Z_DEFAULT_COMPRESSION = -1
Z_FILTERED = 1
Z_HUFFMAN_ONLY = 2
Z_RLE = 3
Z_FIXED = 4
Z_DEFAULT_STRATEGY = 0
Z_NO_FLUSH = 0
Z_PARTIAL_FLUSH = 1
Z_SYNC_FLUSH = 2
Z_FULL_FLUSH = 3
Z_FINISH = 4
Z_BLOCK = 5
Z_TREES = 6
ZLIB_VERSION = "1.3.1"
ZLIB_RUNTIME_VERSION = "1.3.1"

_STATUS_EOF = 1
_STATUS_PENDING = 2
_STATUS_ERROR = 4

_READ_SIZE = 1 << 16


class error(Exception):
    pass


class _Device:
    """One open device file."""

    def __init__(self, path):
        self._fd = -1
        try:
            self._fd = os.open(path, os.O_RDWR)
        except OSError as e:
            raise error(f"zlib device unavailable: {e}") from None

    def _fail(self):
        flags, payload = self.status()
        if flags & _STATUS_ERROR:
            raise error(payload.decode("utf-8", "replace"))
        raise error("zlib device error")

    def write(self, data):
        view = memoryview(data)
        try:
            while len(view):
                n = os.write(self._fd, view)
                view = view[n:]
        except OSError:
            self._fail()

    def read(self, limit=-1):
        chunks = []
        got = 0
        try:
            while limit < 0 or got < limit:
                want = _READ_SIZE if limit < 0 else min(_READ_SIZE, limit - got)
                chunk = os.read(self._fd, want)
                if not chunk:
                    break
                chunks.append(chunk)
                got += len(chunk)
        except OSError:
            self._fail()
        return b"".join(chunks)

    def flush(self):
        try:
            os.fdatasync(self._fd)
        except OSError:
            self._fail()

    def finish(self):
        try:
            os.fsync(self._fd)
        except OSError:
            self._fail()

    def budget(self, n):
        try:
            os.ftruncate(self._fd, n)
        except OSError:
            self._fail()

    def status(self):
        chunks = []
        off = 0
        while True:
            chunk = os.pread(self._fd, _READ_SIZE, off)
            chunks.append(chunk)
            off += len(chunk)
            if len(chunk) < _READ_SIZE:
                break
        rec = b"".join(chunks)
        return rec[0], rec[1:]

    def close(self):
        if self._fd >= 0:
            os.close(self._fd)
            self._fd = -1

    def __del__(self):
        self.close()


def _check_level(level):
    if not (-1 <= level <= 9):
        raise error("Bad compression level")


def _check_wbits(wbits, decompress):
    a = abs(wbits)
    ok = a == 0 and decompress or 8 <= a <= 15 or 24 <= a <= 31 or decompress and 40 <= a <= 47
    if not ok:
        raise error("Invalid initialization option")


def _open(kind, *params, zdict=b""):
    path = "/".join([_DEV, kind, *map(str, params)])
    if zdict:
        dev = _Device(path + "/dict")
        dev.write(zdict)
        dev.flush()
        return dev
    return _Device(path)


def compress(data, /, level=Z_DEFAULT_COMPRESSION, wbits=MAX_WBITS):
    _check_level(level)
    _check_wbits(wbits, False)
    dev = _open("deflate", level, wbits)
    try:
        dev.write(data)
        dev.finish()
        return dev.read()
    finally:
        dev.close()


def decompress(data, /, wbits=MAX_WBITS, bufsize=DEF_BUF_SIZE):
    if bufsize < 0:
        raise ValueError("bufsize must be non-negative")
    _check_wbits(wbits, True)
    dev = _open("inflate", wbits)
    try:
        dev.write(data)
        dev.finish()
        return dev.read()
    finally:
        dev.close()


def crc32(data, value=0):
    return binascii.crc32(data, value)


def adler32(data, value=1):
    dev = _open("adler32", value & 0xFFFFFFFF)
    try:
        dev.write(data)
        _, payload = dev.status()
        return int.from_bytes(payload[:4], "big")
    finally:
        dev.close()


class _Compress:
    def __init__(self, level, method, wbits, memLevel, strategy, zdict):
        if method != DEFLATED:
            raise error("Invalid initialization option")
        _check_level(level)
        _check_wbits(wbits, False)
        self._dev = _open("deflate", level, wbits, zdict=zdict)

    def compress(self, data, /):
        self._dev.write(data)
        return self._dev.read()

    def flush(self, mode=Z_FINISH, /):
        if mode == Z_NO_FLUSH:
            return b""
        if mode == Z_FINISH:
            self._dev.finish()
        else:
            self._dev.flush()
        return self._dev.read()

    def copy(self):
        raise error("copy() is not supported by this zlib implementation")

    __copy__ = copy
    __deepcopy__ = lambda self, memo: self.copy()


def compressobj(level=Z_DEFAULT_COMPRESSION, method=DEFLATED, wbits=MAX_WBITS,
                memLevel=DEF_MEM_LEVEL, strategy=Z_DEFAULT_STRATEGY, zdict=None):
    return _Compress(level, method, wbits, memLevel, strategy, zdict or b"")


class _Decompress:
    """decompressobj().

    The host decoder stops once max_length bytes of output are buffered; the
    input it did not consume comes back in the status record and becomes
    unconsumed_tail, so the usual `while d.unconsumed_tail` loop works.
    """

    def __init__(self, wbits, zdict):
        _check_wbits(wbits, True)
        self._dev = _open("inflate", wbits, zdict=zdict)
        self.eof = False
        self.unused_data = b""
        self.unconsumed_tail = b""

    def _sync(self):
        flags, payload = self._dev.status()
        if flags & _STATUS_EOF and not flags & _STATUS_PENDING:
            self.eof = True
            self.unused_data += payload
            self.unconsumed_tail = b""
        elif flags & _STATUS_EOF:
            self.unconsumed_tail = b""
        else:
            self.unconsumed_tail = payload

    def decompress(self, data, /, max_length=0):
        if max_length < 0:
            raise ValueError("max_length must be non-negative")
        if self.eof:
            self.unused_data += bytes(data)
            return b""
        self._dev.budget(max_length)
        if data:
            self._dev.write(data)
        out = self._dev.read(max_length or -1)
        self._sync()
        return out

    def flush(self, length=DEF_BUF_SIZE, /):
        if length <= 0:
            raise ValueError("length must be greater than zero")
        if self.eof:
            return b""
        self._dev.budget(0)
        if self.unconsumed_tail:
            self._dev.write(self.unconsumed_tail)
        out = self._dev.read()
        self._sync()
        return out

    def copy(self):
        raise error("copy() is not supported by this zlib implementation")

    __copy__ = copy
    __deepcopy__ = lambda self, memo: self.copy()


def decompressobj(wbits=MAX_WBITS, zdict=b""):
    return _Decompress(wbits, zdict)


class _ZlibDecompressor:
    """Decompressor with the bz2/lzma-style needs_input interface (gzip uses it)."""

    def __init__(self, wbits=MAX_WBITS, zdict=b""):
        _check_wbits(wbits, True)
        self._dev = _open("inflate", wbits, zdict=zdict)
        self.eof = False
        self.unused_data = b""
        self.needs_input = True
        self._tail = b""  # input the host handed back, re-fed before new data

    def decompress(self, data, max_length=-1):
        if self.eof:
            raise EOFError("End of stream already reached")
        self._dev.budget(max(max_length, 0))
        pending = self._tail + bytes(data)
        self._tail = b""
        if pending:
            self._dev.write(pending)
        out = self._dev.read(max_length)
        flags, payload = self._dev.status()
        if flags & _STATUS_EOF:
            if not flags & _STATUS_PENDING:
                self.eof = True
                self.unused_data = payload
        else:
            self._tail = payload
        self.needs_input = not self.eof and not (flags & _STATUS_PENDING) and not self._tail
        return out
