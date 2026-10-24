package main

// An aligned zip writer, and the reason zipalign disappears from the build.
//
// zipalign is a second pass over the whole package: it reads the file, moves the data
// of every uncompressed entry to a boundary and rewrites the central directory to
// match. This writer decides the offsets as it writes, so the alignment costs nothing
// and the package is produced once, already signed at the end.
//
// Reuse survives that: an entry whose source did not change has its stored bytes
// copied over as they are, under a fresh local header carrying the padding this file
// needs, which is precisely what zipalign does with the entries it is handed.
//
// The layout is the one the platform expects: local file headers and data, then the
// APK signing block, then the central directory, then the end record.

import (
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	zipVersionNeeded = 20 // 2.0: deflate and the data descriptor both predate it
	zipFlagUTF8      = 0x0800
	zipMethodStore   = 0
	zipMethodDeflate = 8

	sigLocalFile   = 0x04034b50
	sigCentralFile = 0x02014b50
	sigEndCentral  = 0x06054b50

	// where the crc and the two sizes sit inside a local file header, for the patch
	// that follows data whose size was not known when the header went out
	lfhCrc    = 14
	lfhCSize  = 18
	lfhUSize  = 22
	lfhHeader = 30

	// zipalign -f 4 lands every uncompressed entry on a 4-byte boundary, and a .so
	// the manifest wants left in place wants a whole page: the loader maps it out of
	// the package instead of unpacking it
	alignNone     = 1
	alignStandard = 4
	alignPage     = 4096
)

// packedRecord is one entry of the package as written: where its header starts, what
// its bytes are, and the fields the central directory repeats.
type packedRecord struct {
	name     string
	offset   int64
	crc      uint32
	csize    uint32
	usize    uint32
	deflate  bool
	dateTime uint32
}

// previousPackage is the package this one replaces, open for reading: the index says
// where every entry's stored bytes are, and the file is where they are read from. The
// standard reader keeps its own handle private, so this one keeps its own.
type previousPackage struct {
	file    *os.File
	index   map[string]*prevEntry
	names   map[string]bool
	closers []io.Closer
}

func (p *previousPackage) Close() {
	if p == nil || p.file == nil {
		return
	}
	for _, c := range p.closers {
		_ = c.Close()
	}
	p.file.Close()
}

// prevEntry is the reuse information a previous package gives: where the stored bytes
// of an entry are, and what they claim to be. The date travels with it, so a reused
// entry comes out with the bytes it had, not with a new timestamp.
type prevEntry struct {
	dataAt   int64
	dataLen  uint32
	usize    uint32
	crc      uint32
	deflate  bool
	dateTime uint32
}

// writePackageEntries puts every entry of the plan into dst. reuse[i] says the entry
// is unchanged since the previous package, and prev holds that package.
func writePackageEntries(dst *os.File, plan []bundleEntry, reuse []bool, prev *previousPackage) ([]packedRecord, int, error) {
	records := make([]packedRecord, 0, len(plan))
	reused := 0

	for i, e := range plan {
		offset, err := dst.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, 0, err
		}

		var p *prevEntry
		if prev != nil {
			p = prev.index[e.name]
		}
		deflate := e.compress
		boundary := uint16(alignNone)
		if !deflate {
			boundary = alignStandard
			if isNativeLib(e.name) {
				boundary = alignPage
			}
		}
		// the date is written up front, in both the local header and the central
		// directory record: zipalign's checker compares the two against each other
		// and a zero in one of them is a "header mismatch" over every reused entry
		var when uint32
		if reuse[i] && p != nil {
			when = p.dateTime
		} else {
			info, err := os.Stat(e.src)
			if err != nil {
				return nil, 0, err
			}
			when = dosTime(info.ModTime())
		}
		extra := paddingAfter(len(e.name), offset, boundary)
		if _, err := dst.Write(makeHeader(e.name, deflate, extra, 0, 0, 0, when)); err != nil {
			return nil, 0, err
		}

		var rec packedRecord
		if reuse[i] && p != nil {
			rec, err = copyStored(dst, prev.file, e.name, offset, when, p)
		} else {
			rec, err = writeFromFile(dst, e, offset, when)
		}
		if err != nil {
			return nil, 0, err
		}
		if reuse[i] && p != nil {
			reused++
		}
		records = append(records, rec)
	}
	return records, reused, nil
}

// copyStored moves the bytes of an entry the previous package already carried. No
// inflate, no deflate: this is what keeps a one line edit from touching a hundred and
// thirty six megabytes of assets.
func copyStored(dst *os.File, prevFile *os.File, name string, headerOff int64, when uint32, p *prevEntry) (packedRecord, error) {
	if _, err := prevFile.Seek(p.dataAt, io.SeekStart); err != nil {
		return packedRecord{}, err
	}
	n, err := io.Copy(dst, io.LimitReader(prevFile, int64(p.dataLen)))
	if err != nil {
		return packedRecord{}, err
	}
	if uint64(p.dataLen) != uint64(n) {
		return packedRecord{}, io.ErrShortWrite
	}
	// the header was written with these fields left empty: here they are known
	if err := patchHeader(dst, headerOff, p.crc, p.dataLen, p.usize); err != nil {
		return packedRecord{}, err
	}
	return packedRecord{
		name:     name,
		offset:   headerOff,
		crc:      p.crc,
		csize:    p.dataLen,
		usize:    p.usize,
		deflate:  p.deflate,
		dateTime: when,
	}, nil
}

// writeFromFile takes an entry from the tree: deflated when the plan says so, and
// with the crc and the sizes patched into the header once the data is through, which
// is the first moment they can be known.
func writeFromFile(dst *os.File, e bundleEntry, headerOff int64, when uint32) (packedRecord, error) {
	src, err := os.Open(e.src)
	if err != nil {
		return packedRecord{}, err
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return packedRecord{}, err
	}

	counted := &countingWriter{w: dst}
	digest := crc32.NewIEEE()
	source := io.TeeReader(src, digest)
	packed := int64(0)
	if e.compress {
		packer, err := flate.NewWriter(counted, flate.DefaultCompression)
		if err != nil {
			return packedRecord{}, err
		}
		if _, err := io.Copy(packer, source); err != nil {
			return packedRecord{}, err
		}
		// the trailer has to be out before the sizes are known
		if err := packer.Close(); err != nil {
			return packedRecord{}, err
		}
	} else if _, err := io.Copy(counted, source); err != nil {
		return packedRecord{}, err
	}
	packed = counted.n

	crc := digest.Sum32()
	usize := uint32(info.Size())
	csize := uint32(packed)
	if !e.compress {
		// a stored entry is its own compression, and the byte count that went out is
		// the size the record claims
		csize = usize
	}
	if err := patchHeader(dst, headerOff, crc, csize, usize); err != nil {
		return packedRecord{}, err
	}
	return packedRecord{
		name:     e.name,
		offset:   headerOff,
		crc:      crc,
		csize:    csize,
		usize:    usize,
		deflate:  e.compress,
		dateTime: when,
	}, nil
}

func makeHeader(name string, deflate bool, extra, crc, csize, usize, dateTime uint32) []byte {
	method := uint16(zipMethodStore)
	if deflate {
		method = zipMethodDeflate
	}
	head := make([]byte, lfhHeader+len(name)+int(extra))
	binary.LittleEndian.PutUint32(head[0:], sigLocalFile)
	binary.LittleEndian.PutUint16(head[4:], zipVersionNeeded)
	binary.LittleEndian.PutUint16(head[6:], zipFlagUTF8)
	binary.LittleEndian.PutUint16(head[8:], method)
	binary.LittleEndian.PutUint32(head[10:], dateTime)
	binary.LittleEndian.PutUint32(head[lfhCrc:], crc)
	binary.LittleEndian.PutUint32(head[lfhCSize:], csize)
	binary.LittleEndian.PutUint32(head[lfhUSize:], usize)
	binary.LittleEndian.PutUint16(head[26:], uint16(len(name)))
	binary.LittleEndian.PutUint16(head[28:], uint16(extra))
	copy(head[lfhHeader:], name)
	// the padding is zeroes, exactly how zipalign pads: a tag here would claim a
	// property the entry does not have
	return head
}

// patchHeader writes back what a header could not know before its data: the crc and
// the two sizes.
func patchHeader(dst *os.File, headerOff int64, crc, csize, usize uint32) error {
	var buf [12]byte
	binary.LittleEndian.PutUint32(buf[0:], crc)
	binary.LittleEndian.PutUint32(buf[4:], csize)
	binary.LittleEndian.PutUint32(buf[8:], usize)
	_, err := dst.WriteAt(buf[:], headerOff+lfhCrc)
	return err
}

// paddingAfter is the length of the extra field that lands the data on boundary: a
// header of 30 + name + extra comes before it.
func paddingAfter(nameLen int, offset int64, boundary uint16) uint32 {
	if boundary <= 1 {
		return 0
	}
	head := (offset + lfhHeader + int64(nameLen)) % int64(boundary)
	if head == 0 {
		return 0
	}
	return uint32(int64(boundary) - head)
}

// isNativeLib is the name shape the page alignment is for.
func isNativeLib(name string) bool {
	if !strings.HasSuffix(name, ".so") {
		return false
	}
	head := name
	if i := strings.IndexByte(head, filepath.Separator); i >= 0 {
		head = head[:i]
	}
	if head != "lib" {
		return false
	}
	// the entry names of a package are forward-slashed by the zip format itself
	rest := name[len(head):]
	return strings.HasPrefix(rest, string(filepath.Separator)) || strings.HasPrefix(rest, "/")
}

// dosTime is the only clock a zip record carries. Two builds of one content must come
// out the same, so a reused entry keeps the value it already had.
func dosTime(t time.Time) uint32 {
	if t.IsZero() {
		return 0x210000 // 1980-01-01, the epoch of the format
	}
	year := t.Year()
	if year < 1980 {
		return 0x210000
	}
	date := uint32(year-1980)<<9 | uint32(t.Month())<<5 | uint32(t.Day())
	clock := uint32(t.Hour())<<11 | uint32(t.Minute())<<5 | uint32(t.Second()/2)
	return date<<16 | clock
}

// centralDirectory builds the two records that close a zip. Both come back as bytes,
// because the signature digest covers them before a single one is written: the end
// record is hashed with a different value in its central directory offset than the one
// it is finally written with.
func centralDirectory(records []packedRecord, cdOffset uint32) (cd []byte, eocd []byte) {
	cd = make([]byte, 0, len(records)*64)
	for _, r := range records {
		rec := make([]byte, 46+len(r.name))
		binary.LittleEndian.PutUint32(rec[0:], sigCentralFile)
		binary.LittleEndian.PutUint16(rec[4:], 0x031E) // made by UNIX, zip 3.0
		binary.LittleEndian.PutUint16(rec[6:], zipVersionNeeded)
		binary.LittleEndian.PutUint16(rec[8:], zipFlagUTF8)
		if r.deflate {
			binary.LittleEndian.PutUint16(rec[10:], zipMethodDeflate)
		}
		binary.LittleEndian.PutUint32(rec[12:], r.dateTime)
		binary.LittleEndian.PutUint32(rec[16:], r.crc)
		binary.LittleEndian.PutUint32(rec[20:], r.csize)
		binary.LittleEndian.PutUint32(rec[24:], r.usize)
		binary.LittleEndian.PutUint16(rec[28:], uint16(len(r.name)))
		binary.LittleEndian.PutUint32(rec[38:], 0o644<<16)
		binary.LittleEndian.PutUint32(rec[42:], uint32(r.offset))
		copy(rec[46:], r.name)
		cd = append(cd, rec...)
	}

	eocd = make([]byte, 22)
	binary.LittleEndian.PutUint32(eocd[0:], sigEndCentral)
	binary.LittleEndian.PutUint16(eocd[8:], uint16(len(records)))
	binary.LittleEndian.PutUint16(eocd[10:], uint16(len(records)))
	binary.LittleEndian.PutUint32(eocd[12:], uint32(len(cd)))
	binary.LittleEndian.PutUint32(eocd[16:], cdOffset)
	return cd, eocd
}

// countingWriter is how a stream reports how many bytes reached the file: the
// compressed size of an entry is what came out of the deflater, not what went in.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
