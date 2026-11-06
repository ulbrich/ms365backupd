// Package quickxor implements Microsoft's QuickXorHash used by OneDrive / SharePoint
// to report file content hashes. Port of the reference C# implementation:
// https://learn.microsoft.com/onedrive/developer/code-snippets/quickxorhash
package quickxor

import (
	"encoding/base64"
	"encoding/binary"
	"hash"
)

const (
	widthInBits = 160
	shift       = 11
	// Size of the digest in bytes.
	Size = widthInBits / 8
	// BlockSize is nominal; the algorithm is not block based.
	BlockSize = 64
	cells     = (widthInBits-1)/64 + 1
)

type digest struct {
	data        [cells]uint64
	shiftSoFar  int
	lengthSoFar int64
}

// New returns a new hash.Hash computing QuickXorHash.
func New() hash.Hash { return &digest{} }

func (d *digest) Reset()         { *d = digest{} }
func (d *digest) Size() int      { return Size }
func (d *digest) BlockSize() int { return BlockSize }

func (d *digest) Write(p []byte) (int, error) {
	currentShift := d.shiftSoFar
	vectorArrayIndex := currentShift / 64
	vectorOffset := currentShift % 64
	iterations := len(p)
	if iterations > widthInBits {
		iterations = widthInBits
	}
	for i := 0; i < iterations; i++ {
		isLastCell := vectorArrayIndex == cells-1
		bitsInVectorCell := 64
		if isLastCell {
			bitsInVectorCell = widthInBits % 64
		}
		if vectorOffset <= bitsInVectorCell-8 {
			for j := i; j < len(p); j += widthInBits {
				d.data[vectorArrayIndex] ^= uint64(p[j]) << uint(vectorOffset)
			}
		} else {
			index1 := vectorArrayIndex
			index2 := 0
			if !isLastCell {
				index2 = vectorArrayIndex + 1
			}
			low := uint(bitsInVectorCell - vectorOffset)
			var xored byte
			for j := i; j < len(p); j += widthInBits {
				xored ^= p[j]
			}
			d.data[index1] ^= uint64(xored) << uint(vectorOffset)
			d.data[index2] ^= uint64(xored) >> low
		}
		vectorOffset += shift
		for vectorOffset >= bitsInVectorCell {
			if isLastCell {
				vectorArrayIndex = 0
			} else {
				vectorArrayIndex++
			}
			vectorOffset -= bitsInVectorCell
			isLastCell = vectorArrayIndex == cells-1
			bitsInVectorCell = 64
			if isLastCell {
				bitsInVectorCell = widthInBits % 64
			}
		}
	}
	d.shiftSoFar = (d.shiftSoFar + shift*(len(p)%widthInBits)) % widthInBits
	d.lengthSoFar += int64(len(p))
	return len(p), nil
}

func (d *digest) Sum(b []byte) []byte {
	var out [Size]byte
	var buf [8]byte
	for i := 0; i < cells-1; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], d.data[i])
	}
	binary.LittleEndian.PutUint64(buf[:], d.data[cells-1])
	copy(out[(cells-1)*8:], buf[:Size-(cells-1)*8])
	binary.LittleEndian.PutUint64(buf[:], uint64(d.lengthSoFar))
	for i := 0; i < 8; i++ {
		out[Size-8+i] ^= buf[i]
	}
	return append(b, out[:]...)
}

// Base64 returns the digest encoded the way the Graph API reports it.
func Base64(h hash.Hash) string { return base64.StdEncoding.EncodeToString(h.Sum(nil)) }
