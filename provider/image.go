package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"strings"
)

func repairImageDataURL(url string) string {
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(url, prefix) {
		return url
	}
	data, err := base64.StdEncoding.DecodeString(strings.Map(dropWhitespace, url[len(prefix):]))
	if err != nil {
		return url
	}
	fixed := repairPNGChunks(data)
	if fixed == nil {
		return url
	}
	return prefix + base64.StdEncoding.EncodeToString(fixed)
}

func dropWhitespace(r rune) rune {
	switch r {
	case ' ', '\t', '\n', '\r':
		return -1
	}
	return r
}

func repairPNGChunks(data []byte) []byte {
	sig := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if !bytes.HasPrefix(data, sig) {
		return nil
	}

	out := make([]byte, 0, len(data))
	out = append(out, sig...)
	pos := 8
	changed := false

	for pos+8 <= len(data) {
		length := binary.BigEndian.Uint32(data[pos : pos+4])
		chunkType := data[pos+4 : pos+8]
		dataStart := pos + 8
		dataEnd := dataStart + int(length)
		if dataEnd+4 > len(data) {
			return nil
		}

		chunkData := data[dataStart:dataEnd]
		stored := binary.BigEndian.Uint32(data[dataEnd : dataEnd+4])

		crc := crc32.New(crc32.IEEETable)
		_, _ = crc.Write(chunkType)
		_, _ = crc.Write(chunkData)
		want := crc.Sum32()

		if stored == want {
			out = append(out, data[pos:dataEnd+4]...)
		} else {
			changed = true
			out = append(out, data[pos:dataEnd]...)
			var crcBytes [4]byte
			binary.BigEndian.PutUint32(crcBytes[:], want)
			out = append(out, crcBytes[:]...)
		}
		pos = dataEnd + 4
	}

	if !changed {
		return nil
	}
	return out
}
