#!/usr/bin/env python3
# Minimal CSO v1 writer (raw deflate per 2048-byte block): mkcso.py in.iso out.cso. Used to test the CSO path with an own image.
import struct, sys, zlib
src = open(sys.argv[1], 'rb').read(); bs = 2048; n = (len(src) + bs - 1) // bs
src += b'\0' * (n * bs - len(src)); blocks = []
for i in range(n):
    b = src[i * bs:(i + 1) * bs]; c = zlib.compressobj(9, zlib.DEFLATED, -15); z = c.compress(b) + c.flush()
    blocks.append((z, False) if len(z) < bs else (b, True))
off = 24 + (n + 1) * 4; idx = []
for z, raw in blocks:
    idx.append(off | (0x80000000 if raw else 0)); off += len(z)
idx.append(off)
open(sys.argv[2], 'wb').write(struct.pack('<4sIQIBBH', b'CISO', 24, len(src), bs, 1, 0, 0) + b''.join(struct.pack('<I', x) for x in idx) + b''.join(z for z, _ in blocks))
