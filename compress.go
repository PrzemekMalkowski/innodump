// compress.go: optional zstd compression for --format=tsv's data files, in
// the same form MySQL Shell's own dump utility writes (a table's data file
// gets a plain zstd frame with a ".zst" suffix added to its "extension", and
// its own metadata JSON records "compression": "zstd" - see tsvdump.go's
// AddTable). Nothing else in a dump directory is ever compressed - DDL and
// metadata files are always plain text, exactly as a real dump's are.
//
// Compression is opt-in (--compression=none by default) even though
// util.dumpSchemas() itself defaults to zstd - a single-table extraction
// tool producing a surprise .zst file nobody asked for seemed like the
// wrong default here, but the resulting file is fully util.loadDump()
// -compatible either way.
//
// This uses klauspost/compress/zstd, a pure-Go encoder, rather than
// bindings to the real libzstd - so the file this tool writes is a
// completely standard, valid zstd frame (any zstd decoder, including the
// one util.loadDump() itself uses, reads it identically to one libzstd
// produced), but --compression-level's number - the same real 1-22 zstd
// level MySQL Shell's own compressionLevel option takes - only selects the
// nearest of this encoder's four internal speed/ratio presets
// (zstd.EncoderLevelFromZstd), rather than that exact numbered level.
package main

import (
	"bufio"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// compressionKind is a table data file's on-disk compression.
type compressionKind int

const (
	compressionNone compressionKind = iota
	compressionZstd
)

// parseCompressionKind validates --compression's value.
func parseCompressionKind(s string) (compressionKind, error) {
	switch s {
	case "none", "":
		return compressionNone, nil
	case "zstd":
		return compressionZstd, nil
	default:
		return compressionNone, fmt.Errorf(`--compression must be "none" or "zstd", not %q`, s)
	}
}

// extension is the suffix this data file's basename+".tsv" grows were this
// compression used, and what a table's own metadata JSON records under
// "extension" (see tsvTableMeta) - "tsv" or "tsv.zst".
func (c compressionKind) extension() string {
	if c == compressionZstd {
		return "tsv.zst"
	}
	return "tsv"
}

// metadataName is what a table's own metadata JSON records under
// "compression" (see tsvTableMeta) - "none" or "zstd".
func (c compressionKind) metadataName() string {
	if c == compressionZstd {
		return "zstd"
	}
	return "none"
}

func (c compressionKind) String() string {
	if c == compressionZstd {
		return "zstd"
	}
	return "none"
}

// dataWriter is the common interface a table's data file is written
// through, whether or not it's compressed: Finish flushes (plain) or
// finalizes the zstd frame (compressed), but - either way - never closes
// the underlying file, which the caller still closes itself.
type dataWriter interface {
	io.Writer
	Finish() error
}

type bufDataWriter struct{ *bufio.Writer }

func (w bufDataWriter) Finish() error { return w.Flush() }

type zstdDataWriter struct{ *zstd.Encoder }

func (w zstdDataWriter) Finish() error { return w.Close() }

// newDataWriter wraps f for buffered (compression == compressionNone) or
// zstd-compressed (compressionZstd) writing. level is a real zstd
// compression level (1-22); see this file's package comment for how that
// maps onto the pure-Go encoder actually used.
func newDataWriter(f io.Writer, compression compressionKind, level int) (dataWriter, error) {
	if compression == compressionZstd {
		enc, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)))
		if err != nil {
			return nil, fmt.Errorf("initializing zstd compression: %w", err)
		}
		return zstdDataWriter{enc}, nil
	}
	// Buffered even when uncompressed: at millions of rows, one write(2)
	// syscall per row otherwise dominates wall-clock time far more than
	// any of the actual decoding does - see btree.go's WalkRows.
	return bufDataWriter{bufio.NewWriterSize(f, 1<<20)}, nil
}
