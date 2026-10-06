// Package rpm reads the identifying fields of an RPM package header.
package rpm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
)

// Header holds the NEVRA of a package and whether it is a source package.
type Header struct {
	Name    string
	Epoch   string
	Version string
	Release string
	Arch    string
	Source  bool
}

const (
	tagName      = 1000
	tagVersion   = 1001
	tagRelease   = 1002
	tagEpoch     = 1003
	tagArch      = 1022
	tagSourceRPM = 1044

	typeInt32  = 4
	typeString = 6
)

type entry struct {
	typ    uint32
	offset uint32
}

// ReadHeader parses the lead, signature header and main header of an RPM file.
func ReadHeader(data []byte) (Header, error) {
	if len(data) < 96 || !bytes.Equal(data[:4], []byte{0xed, 0xab, 0xee, 0xdb}) {
		return Header{}, fmt.Errorf("not an RPM file")
	}
	_, _, sigLen, err := parseHeader(data, 96)
	if err != nil {
		return Header{}, fmt.Errorf("signature header: %w", err)
	}
	mainOff := 96 + sigLen + (8-sigLen%8)%8
	entries, store, _, err := parseHeader(data, mainOff)
	if err != nil {
		return Header{}, fmt.Errorf("main header: %w", err)
	}

	h := Header{
		Name:    readString(entries, store, tagName),
		Version: readString(entries, store, tagVersion),
		Release: readString(entries, store, tagRelease),
		Arch:    readString(entries, store, tagArch),
	}
	if e, ok := entries[tagEpoch]; ok && e.typ == typeInt32 && int(e.offset)+4 <= len(store) {
		h.Epoch = strconv.FormatUint(uint64(binary.BigEndian.Uint32(store[e.offset:])), 10)
	}
	_, hasSourceRPM := entries[tagSourceRPM]
	h.Source = !hasSourceRPM
	if h.Name == "" || h.Version == "" || h.Release == "" || h.Arch == "" {
		return Header{}, fmt.Errorf("incomplete RPM header")
	}
	return h, nil
}

func parseHeader(data []byte, off int) (map[uint32]entry, []byte, int, error) {
	if len(data) < off+16 || !bytes.Equal(data[off:off+3], []byte{0x8e, 0xad, 0xe8}) {
		return nil, nil, 0, fmt.Errorf("bad header magic")
	}
	count := int(binary.BigEndian.Uint32(data[off+8:]))
	size := int(binary.BigEndian.Uint32(data[off+12:]))
	storeStart := off + 16 + 16*count
	end := storeStart + size
	if count < 0 || size < 0 || end > len(data) {
		return nil, nil, 0, fmt.Errorf("truncated header")
	}
	entries := make(map[uint32]entry, count)
	for i := 0; i < count; i++ {
		e := data[off+16+16*i:]
		entries[binary.BigEndian.Uint32(e)] = entry{
			typ:    binary.BigEndian.Uint32(e[4:]),
			offset: binary.BigEndian.Uint32(e[8:]),
		}
	}
	return entries, data[storeStart:end], end - off, nil
}

func readString(entries map[uint32]entry, store []byte, tag uint32) string {
	e, ok := entries[tag]
	if !ok || e.typ != typeString || int(e.offset) >= len(store) {
		return ""
	}
	s := store[e.offset:]
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return string(s)
}
