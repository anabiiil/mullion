//go:build ignore

// icons.go packs the rendered icon frames into mullion.ico and swaps them
// into rsrc_windows_amd64.syso — the COFF resource object the Windows build
// links in (exe icon in Explorer/taskbar, and the tray icon, which reads it
// back out of the exe). Every other resource in the .syso (version info,
// manifest) is carried over byte for byte, so this doesn't need
// goversioninfo or any other tool: run it via tools/brand/build.sh.
//
//	go run tools/brand/icons.go <ico-png-dir> <repo-root>
//
// <ico-png-dir> holds icon-<N>.png frames (written by winicon.go). Frames
// are stored as PNG inside the .ico, which Windows Vista+ reads natively.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"sort"
)

var sizes = []int{16, 24, 32, 48, 64, 128, 256}

const (
	rtIcon      = 3
	rtGroupIcon = 14
)

type frame struct {
	size int
	data []byte
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run tools/brand/icons.go <ico-png-dir> <repo-root>")
		os.Exit(2)
	}
	pngDir, root := os.Args[1], os.Args[2]

	var frames []frame
	for _, s := range sizes {
		data, err := os.ReadFile(filepath.Join(pngDir, fmt.Sprintf("icon-%d.png", s)))
		check(err)
		img, err := png.Decode(bytes.NewReader(data))
		check(err)
		if b := img.Bounds(); b.Dx() != s || b.Dy() != s {
			check(fmt.Errorf("icon-%d.png is %dx%d", s, b.Dx(), b.Dy()))
		}
		frames = append(frames, frame{s, data})
	}

	icoPath := filepath.Join(root, "mullion.ico")
	check(os.WriteFile(icoPath, buildICO(frames), 0o644))
	fmt.Println("wrote", icoPath)

	sysoPath := filepath.Join(root, "rsrc_windows_amd64.syso")
	old, err := os.ReadFile(sysoPath)
	check(err)
	leaves, symtab := parseSyso(old)

	// Drop every old icon, keep each icon group's resource name/language
	// (goversioninfo writes the group twice, as 2 and as 32512), and add the
	// new frames as RT_ICON 1..N shared by all of those groups.
	type groupID struct{ name, lang uint32 }
	var groups []groupID
	var kept []leaf
	for _, l := range leaves {
		switch l.typ {
		case rtIcon:
			continue
		case rtGroupIcon:
			groups = append(groups, groupID{l.name, l.lang})
			continue
		}
		kept = append(kept, l)
	}
	if len(groups) == 0 {
		groups = []groupID{{1, 0x409}}
	}
	for i, f := range frames {
		kept = append(kept, leaf{rtIcon, uint32(i + 1), groups[0].lang, f.data})
	}
	for _, g := range groups {
		kept = append(kept, leaf{rtGroupIcon, g.name, g.lang, buildGroup(frames)})
	}
	check(os.WriteFile(sysoPath, buildSyso(kept, symtab), 0o644))
	fmt.Printf("wrote %s (%d resources)\n", sysoPath, len(kept))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func dim(s int) byte {
	if s >= 256 {
		return 0 // 0 means 256 in ICONDIRENTRY / GRPICONDIRENTRY
	}
	return byte(s)
}

// buildICO writes an ICONDIR + ICONDIRENTRY table followed by the PNGs.
func buildICO(frames []frame) []byte {
	var b bytes.Buffer
	le := binary.LittleEndian
	binary.Write(&b, le, [3]uint16{0, 1, uint16(len(frames))})
	off := 6 + 16*len(frames)
	for _, f := range frames {
		b.Write([]byte{dim(f.size), dim(f.size), 0, 0})
		binary.Write(&b, le, [2]uint16{1, 32})
		binary.Write(&b, le, [2]uint32{uint32(len(f.data)), uint32(off)})
		off += len(f.data)
	}
	for _, f := range frames {
		b.Write(f.data)
	}
	return b.Bytes()
}

// buildGroup writes the RT_GROUP_ICON payload: like an ICONDIR, but each
// entry ends with the RT_ICON resource id instead of a file offset.
func buildGroup(frames []frame) []byte {
	var b bytes.Buffer
	le := binary.LittleEndian
	binary.Write(&b, le, [3]uint16{0, 1, uint16(len(frames))})
	for i, f := range frames {
		b.Write([]byte{dim(f.size), dim(f.size), 0, 0})
		binary.Write(&b, le, [2]uint16{1, 32})
		binary.Write(&b, le, uint32(len(f.data)))
		binary.Write(&b, le, uint16(i+1))
	}
	return b.Bytes()
}

// ── COFF .syso ────────────────────────────────────────────────────────

type leaf struct {
	typ, name, lang uint32
	data            []byte
}

// parseSyso reads a single-section (.rsrc) COFF object as written by
// goversioninfo/rsrc: a three-level ID-only resource tree whose data
// entries hold section-relative offsets fixed up by ADDR32NB relocations.
// It returns every leaf plus the raw symbol/string table to carry over.
func parseSyso(b []byte) ([]leaf, []byte) {
	le := binary.LittleEndian
	if le.Uint16(b[0:]) != 0x8664 || le.Uint16(b[2:]) != 1 {
		check(fmt.Errorf("unexpected .syso header (machine %#x, %d sections)", le.Uint16(b[0:]), le.Uint16(b[2:])))
	}
	symPtr := le.Uint32(b[8:])
	sec := b[20:]
	if string(bytes.TrimRight(sec[:8], "\x00")) != ".rsrc" {
		check(fmt.Errorf("first section is %q, not .rsrc", sec[:8]))
	}
	rawSize, rawPtr := le.Uint32(sec[16:]), le.Uint32(sec[20:])
	rs := b[rawPtr : rawPtr+rawSize]

	var leaves []leaf
	var walk func(off uint32, depth int, ids [3]uint32)
	walk = func(off uint32, depth int, ids [3]uint32) {
		named, idn := le.Uint16(rs[off+12:]), le.Uint16(rs[off+14:])
		if named != 0 {
			check(fmt.Errorf("named resource entries are not supported"))
		}
		for i := 0; i < int(idn); i++ {
			e := rs[off+16+uint32(i)*8:]
			id, target := le.Uint32(e), le.Uint32(e[4:])
			ids[depth] = id
			if target&0x80000000 != 0 {
				walk(target&0x7fffffff, depth+1, ids)
				continue
			}
			if depth != 2 {
				check(fmt.Errorf("data entry at depth %d", depth))
			}
			d := rs[target:]
			dataOff, size := le.Uint32(d), le.Uint32(d[4:])
			leaves = append(leaves, leaf{ids[0], ids[1], ids[2], append([]byte(nil), rs[dataOff:dataOff+size]...)})
		}
	}
	walk(0, 0, [3]uint32{})
	return leaves, append([]byte(nil), b[symPtr:]...)
}

// buildSyso lays the resources out as: directory tables, data entries,
// then 8-aligned data blobs; one ADDR32NB relocation per data entry
// against symbol 0 (the .rsrc section symbol, carried over from the old
// object).
func buildSyso(leaves []leaf, symtab []byte) []byte {
	le := binary.LittleEndian
	sort.Slice(leaves, func(i, j int) bool {
		a, b := leaves[i], leaves[j]
		if a.typ != b.typ {
			return a.typ < b.typ
		}
		if a.name != b.name {
			return a.name < b.name
		}
		return a.lang < b.lang
	})

	// Group into type -> name -> langs.
	type nameGroup struct {
		name  uint32
		langs []int // indexes into leaves
	}
	type typeGroup struct {
		typ   uint32
		names []*nameGroup
	}
	var types []*typeGroup
	for i, l := range leaves {
		if len(types) == 0 || types[len(types)-1].typ != l.typ {
			types = append(types, &typeGroup{typ: l.typ})
		}
		t := types[len(types)-1]
		if len(t.names) == 0 || t.names[len(t.names)-1].name != l.name {
			t.names = append(t.names, &nameGroup{name: l.name})
		}
		n := t.names[len(t.names)-1]
		n.langs = append(n.langs, i)
	}

	dirSize := func(n int) uint32 { return 16 + 8*uint32(n) }
	// Offsets: root, then each type dir, then each name dir.
	off := dirSize(len(types))
	typeOff := make([]uint32, len(types))
	for i, t := range types {
		typeOff[i] = off
		off += dirSize(len(t.names))
	}
	nameOff := map[*nameGroup]uint32{}
	for _, t := range types {
		for _, n := range t.names {
			nameOff[n] = off
			off += dirSize(len(n.langs))
		}
	}
	entryOff := off
	off += 16 * uint32(len(leaves))
	dataOff := make([]uint32, len(leaves))
	for i, l := range leaves {
		off = (off + 7) &^ 7
		dataOff[i] = off
		off += uint32(len(l.data))
	}
	rs := make([]byte, (off+7)&^7)

	putDir := func(at uint32, n int) {
		le.PutUint16(rs[at+14:], uint16(n))
	}
	putEntry := func(dir uint32, i int, id, target uint32) {
		e := rs[dir+16+uint32(i)*8:]
		le.PutUint32(e, id)
		le.PutUint32(e[4:], target)
	}
	putDir(0, len(types))
	for i, t := range types {
		putEntry(0, i, t.typ, typeOff[i]|0x80000000)
		putDir(typeOff[i], len(t.names))
		for j, n := range t.names {
			putEntry(typeOff[i], j, n.name, nameOff[n]|0x80000000)
			putDir(nameOff[n], len(n.langs))
			for k, li := range n.langs {
				putEntry(nameOff[n], k, leaves[li].lang, entryOff+16*uint32(li))
			}
		}
	}
	var relocs bytes.Buffer
	for i, l := range leaves {
		e := rs[entryOff+16*uint32(i):]
		le.PutUint32(e, dataOff[i])
		le.PutUint32(e[4:], uint32(len(l.data)))
		copy(rs[dataOff[i]:], l.data)
		binary.Write(&relocs, le, uint32(entryOff+16*uint32(i))) // VirtualAddress
		binary.Write(&relocs, le, uint32(0))                     // SymbolTableIndex
		binary.Write(&relocs, le, uint16(3))                     // IMAGE_REL_AMD64_ADDR32NB
	}

	const hdr, secHdr = 20, 40
	rawPtr := uint32(hdr + secHdr)
	relPtr := rawPtr + uint32(len(rs))
	symPtr := relPtr + uint32(relocs.Len())

	var out bytes.Buffer
	binary.Write(&out, le, uint16(0x8664))
	binary.Write(&out, le, uint16(1))
	binary.Write(&out, le, uint32(0))
	binary.Write(&out, le, symPtr)
	binary.Write(&out, le, uint32(1)) // one symbol: .rsrc
	binary.Write(&out, le, uint16(0))
	binary.Write(&out, le, uint16(0x0104)) // LINE_NUMS_STRIPPED | 32BIT_MACHINE, as goversioninfo writes

	name := [8]byte{'.', 'r', 's', 'r', 'c'}
	out.Write(name[:])
	binary.Write(&out, le, uint32(0))           // VirtualSize
	binary.Write(&out, le, uint32(0))           // VirtualAddress
	binary.Write(&out, le, uint32(len(rs)))     // SizeOfRawData
	binary.Write(&out, le, rawPtr)              // PointerToRawData
	binary.Write(&out, le, relPtr)              // PointerToRelocations
	binary.Write(&out, le, uint32(0))           // PointerToLinenumbers
	binary.Write(&out, le, uint16(len(leaves))) // NumberOfRelocations
	binary.Write(&out, le, uint16(0))           // NumberOfLinenumbers
	binary.Write(&out, le, uint32(0x40000040))  // INITIALIZED_DATA | MEM_READ

	out.Write(rs)
	out.Write(relocs.Bytes())
	out.Write(symtab)
	return out.Bytes()
}
