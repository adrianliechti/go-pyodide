package pyodide

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"strconv"

	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
)

// AES devices live at crypto/aes/{ecb,cbc}/{encrypt,decrypt}/{key bytes}.
// Write the key followed by the CBC IV, then Datasync to initialize. Further
// writes feed unpadded data and reads drain the result. Sync finishes the
// stream, rejecting a partial final block. Keys never appear in file paths.
// The Python side drains output after each chunk; host buffering is bounded
// even if another guest writes without reading.
const aesBufferLimit = 64 << 10

type aesDevice struct {
	keySize int
	cbc     bool
	decrypt bool
	setup   []byte
	block   cipher.Block
	mode    cipher.BlockMode
	pending [aes.BlockSize]byte
	n       int
	out     bytes.Buffer
	err     error
	done    bool
	closed  bool
}

func openCryptoDevice(args []string) (device, experimentalsys.Errno) {
	if len(args) != 4 || args[0] != "aes" ||
		(args[1] != "ecb" && args[1] != "cbc") ||
		(args[2] != "encrypt" && args[2] != "decrypt") {
		return nil, experimentalsys.ENOENT
	}
	n, err := strconv.Atoi(args[3])
	if err != nil || (n != 16 && n != 24 && n != 32) {
		return nil, experimentalsys.EINVAL
	}
	return &aesDevice{keySize: n, cbc: args[1] == "cbc", decrypt: args[2] == "decrypt"}, 0
}

func (d *aesDevice) fail(message string) experimentalsys.Errno {
	d.err = errors.New(message)
	return experimentalsys.EINVAL
}

func (d *aesDevice) setupSize() int {
	if d.cbc {
		return d.keySize + aes.BlockSize
	}
	return d.keySize
}

func (d *aesDevice) Write(p []byte) (int, experimentalsys.Errno) {
	if d.closed || d.done || d.err != nil {
		return 0, experimentalsys.EIO
	}
	if d.block == nil {
		if len(p) > d.setupSize()-len(d.setup) {
			return 0, d.fail("invalid AES key/IV length")
		}
		d.setup = append(d.setup, p...)
		return len(p), 0
	}
	// Refuse oversized writes before allocating, without consuming input or
	// changing CBC state. A caller can drain output and retry a smaller chunk.
	if len(p) > aesBufferLimit || (d.n+len(p))/aes.BlockSize*aes.BlockSize > aesBufferLimit-d.out.Len() {
		return 0, experimentalsys.EAGAIN
	}
	buf := make([]byte, d.n+len(p))
	copy(buf, d.pending[:d.n])
	copy(buf[d.n:], p)
	n := len(buf) / aes.BlockSize * aes.BlockSize
	clear(d.pending[:])
	d.n = copy(d.pending[:], buf[n:])
	if d.mode != nil {
		d.mode.CryptBlocks(buf[:n], buf[:n])
	} else {
		for i := 0; i < n; i += aes.BlockSize {
			block := buf[i : i+aes.BlockSize]
			if d.decrypt {
				d.block.Decrypt(block, block)
			} else {
				d.block.Encrypt(block, block)
			}
		}
	}
	_, _ = d.out.Write(buf[:n])
	clear(buf)
	return len(p), 0
}

func (d *aesDevice) Datasync() experimentalsys.Errno {
	if d.closed || d.done || d.err != nil {
		return experimentalsys.EIO
	}
	if d.block != nil {
		return 0
	}
	if len(d.setup) != d.setupSize() {
		return d.fail("invalid AES key/IV length")
	}
	block, err := aes.NewCipher(d.setup[:d.keySize])
	if err != nil {
		return d.fail("invalid AES key length")
	}
	d.block = block
	if d.cbc {
		iv := d.setup[d.keySize:]
		if d.decrypt {
			d.mode = cipher.NewCBCDecrypter(block, iv)
		} else {
			d.mode = cipher.NewCBCEncrypter(block, iv)
		}
	}
	clear(d.setup)
	d.setup = nil
	return 0
}

func (d *aesDevice) Read(p []byte) (int, experimentalsys.Errno) {
	if d.closed || d.err != nil {
		return 0, experimentalsys.EIO
	}
	n := copy(p, d.out.Bytes())
	clear(d.out.Next(n))
	return n, 0
}

func (d *aesDevice) Sync() experimentalsys.Errno {
	if d.closed || d.err != nil {
		return experimentalsys.EIO
	}
	if d.block == nil {
		return d.fail("AES device is not initialized")
	}
	if d.n != 0 {
		return d.fail("AES data length must be a multiple of 16")
	}
	d.done = true
	return 0
}

func (d *aesDevice) Truncate(int64) experimentalsys.Errno { return experimentalsys.ENOTSUP }

func (d *aesDevice) Status() []byte {
	return status(d.done, d.out.Len() != 0, d.err, nil)
}

func (d *aesDevice) Close() experimentalsys.Errno {
	clear(d.setup)
	clear(d.pending[:])
	clear(d.out.Bytes())
	d.setup, d.block, d.mode = nil, nil, nil
	d.out.Reset()
	d.n = 0
	d.closed = true
	return 0
}
