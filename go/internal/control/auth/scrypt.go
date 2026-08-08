// Package auth — this file carries the RFC 7914 scrypt mixing function kept for
// compatibility with the retired Python control service. It is adapted from the Go
// Authors' BSD-licensed x/crypto/scrypt implementation to use the standard-library
// crypto/pbkdf2 package.
package auth

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/bits"
)

const maxInt = int(^uint(0) >> 1)

func blockCopy(dst, src []uint32, n int) { copy(dst, src[:n]) }

func blockXOR(dst, src []uint32, n int) {
	for i, value := range src[:n] {
		dst[i] ^= value
	}
}

func salsaXOR(tmp *[16]uint32, in, out []uint32) {
	w0, w1, w2, w3 := tmp[0]^in[0], tmp[1]^in[1], tmp[2]^in[2], tmp[3]^in[3]
	w4, w5, w6, w7 := tmp[4]^in[4], tmp[5]^in[5], tmp[6]^in[6], tmp[7]^in[7]
	w8, w9, w10, w11 := tmp[8]^in[8], tmp[9]^in[9], tmp[10]^in[10], tmp[11]^in[11]
	w12, w13, w14, w15 := tmp[12]^in[12], tmp[13]^in[13], tmp[14]^in[14], tmp[15]^in[15]
	x0, x1, x2, x3 := w0, w1, w2, w3
	x4, x5, x6, x7 := w4, w5, w6, w7
	x8, x9, x10, x11 := w8, w9, w10, w11
	x12, x13, x14, x15 := w12, w13, w14, w15

	for i := 0; i < 8; i += 2 {
		x4 ^= bits.RotateLeft32(x0+x12, 7)
		x8 ^= bits.RotateLeft32(x4+x0, 9)
		x12 ^= bits.RotateLeft32(x8+x4, 13)
		x0 ^= bits.RotateLeft32(x12+x8, 18)
		x9 ^= bits.RotateLeft32(x5+x1, 7)
		x13 ^= bits.RotateLeft32(x9+x5, 9)
		x1 ^= bits.RotateLeft32(x13+x9, 13)
		x5 ^= bits.RotateLeft32(x1+x13, 18)
		x14 ^= bits.RotateLeft32(x10+x6, 7)
		x2 ^= bits.RotateLeft32(x14+x10, 9)
		x6 ^= bits.RotateLeft32(x2+x14, 13)
		x10 ^= bits.RotateLeft32(x6+x2, 18)
		x3 ^= bits.RotateLeft32(x15+x11, 7)
		x7 ^= bits.RotateLeft32(x3+x15, 9)
		x11 ^= bits.RotateLeft32(x7+x3, 13)
		x15 ^= bits.RotateLeft32(x11+x7, 18)
		x1 ^= bits.RotateLeft32(x0+x3, 7)
		x2 ^= bits.RotateLeft32(x1+x0, 9)
		x3 ^= bits.RotateLeft32(x2+x1, 13)
		x0 ^= bits.RotateLeft32(x3+x2, 18)
		x6 ^= bits.RotateLeft32(x5+x4, 7)
		x7 ^= bits.RotateLeft32(x6+x5, 9)
		x4 ^= bits.RotateLeft32(x7+x6, 13)
		x5 ^= bits.RotateLeft32(x4+x7, 18)
		x11 ^= bits.RotateLeft32(x10+x9, 7)
		x8 ^= bits.RotateLeft32(x11+x10, 9)
		x9 ^= bits.RotateLeft32(x8+x11, 13)
		x10 ^= bits.RotateLeft32(x9+x8, 18)
		x12 ^= bits.RotateLeft32(x15+x14, 7)
		x13 ^= bits.RotateLeft32(x12+x15, 9)
		x14 ^= bits.RotateLeft32(x13+x12, 13)
		x15 ^= bits.RotateLeft32(x14+x13, 18)
	}
	values := [...]uint32{
		x0 + w0, x1 + w1, x2 + w2, x3 + w3,
		x4 + w4, x5 + w5, x6 + w6, x7 + w7,
		x8 + w8, x9 + w9, x10 + w10, x11 + w11,
		x12 + w12, x13 + w13, x14 + w14, x15 + w15,
	}
	for index, value := range values {
		out[index], tmp[index] = value, value
	}
}

func blockMix(tmp *[16]uint32, in, out []uint32, r int) {
	blockCopy(tmp[:], in[(2*r-1)*16:], 16)
	for i := 0; i < 2*r; i += 2 {
		salsaXOR(tmp, in[i*16:], out[i*8:])
		salsaXOR(tmp, in[i*16+16:], out[i*8+r*16:])
	}
}

func integer(block []uint32, r int) uint64 {
	index := (2*r - 1) * 16
	return uint64(block[index]) | uint64(block[index+1])<<32
}

func scryptMix(block []byte, r, n int, v, xy []uint32) {
	var tmp [16]uint32
	width := 32 * r
	x, y := xy, xy[width:]
	offset := 0
	for i := 0; i < width; i++ {
		x[i] = binary.LittleEndian.Uint32(block[offset:])
		offset += 4
	}
	for i := 0; i < n; i += 2 {
		blockCopy(v[i*width:], x, width)
		blockMix(&tmp, x, y, r)
		blockCopy(v[(i+1)*width:], y, width)
		blockMix(&tmp, y, x, r)
	}
	for i := 0; i < n; i += 2 {
		index := int(integer(x, r) & uint64(n-1))
		blockXOR(x, v[index*width:], width)
		blockMix(&tmp, x, y, r)
		index = int(integer(y, r) & uint64(n-1))
		blockXOR(y, v[index*width:], width)
		blockMix(&tmp, y, x, r)
	}
	offset = 0
	for _, value := range x[:width] {
		binary.LittleEndian.PutUint32(block[offset:], value)
		offset += 4
	}
}

func deriveScrypt(password string, salt []byte, n, r, p, keyLen int) ([]byte, error) {
	if n <= 1 || n&(n-1) != 0 {
		return nil, errors.New("scrypt: N must be greater than one and a power of two")
	}
	if r <= 0 || p <= 0 {
		return nil, errors.New("scrypt: parameters must be positive")
	}
	if uint64(r)*uint64(p) >= 1<<30 || r > maxInt/128/p ||
		r > maxInt/256 || n > maxInt/128/r {
		return nil, errors.New("scrypt: parameters are too large")
	}
	xy := make([]uint32, 64*r)
	v := make([]uint32, 32*n*r)
	block, err := pbkdf2.Key(sha256.New, password, salt, 1, p*128*r)
	if err != nil {
		return nil, err
	}
	for i := 0; i < p; i++ {
		scryptMix(block[i*128*r:], r, n, v, xy)
	}
	return pbkdf2.Key(sha256.New, password, block, 1, keyLen)
}
