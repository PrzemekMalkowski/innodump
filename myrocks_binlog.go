// myrocks_binlog.go: resolving MyRocks' two-phase-commit transactions
// against the binary log, the way the server's own crash recovery does.
//
// With the binlog enabled, MyRocks commits every transaction in two
// phases: its writes go into the RocksDB WAL as a PREPARE section first,
// the transaction is written to the binlog, and only then is a COMMIT
// marker added to the WAL - which MyRocks doesn't even flush to disk
// straight away, since the binlog alone is enough to decide. So a WAL
// copied from a crashed (or still running) server routinely ends with
// prepared sections whose COMMIT marker never made it to the file. On
// restart the server commits exactly those whose XID made it into the
// binlog (an XID_EVENT), and rolls the rest back; this does the same.
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	binlogXIDEvent                = 16
	binlogTransactionPayloadEvent = 40
	binlogEventHeaderLen          = 19
)

var binlogMagic = []byte{0xfe, 'b', 'i', 'n'}

// mysqlXIDNumber extracts my_xid from a MyRocks transaction name
// (rdb_xid_to_string: 8-byte formatID, gtrid and bqual lengths, then the
// XID data - "MySQLXid", the 8-byte server id, the 8-byte my_xid).
func mysqlXIDNumber(name []byte) (uint64, bool) {
	if len(name) < 10 {
		return 0, false
	}
	gl, bl := int(name[8]), int(name[9])
	data := name[10:]
	if len(data) < gl+bl || gl < 24 || string(data[:8]) != "MySQLXid" {
		return 0, false
	}
	return binary.LittleEndian.Uint64(data[16:24]), true
}

// findBinlogs lists the binary log files of the instance whose datadir is
// datadir: those its *.index file names, else any file there starting
// with the binlog magic number and ending in a 6-digit sequence number.
func findBinlogs(datadir string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if seen[p] {
			return
		}
		if isBinlogFile(p) {
			seen[p] = true
			out = append(out, p)
		}
	}
	idx, _ := filepath.Glob(filepath.Join(datadir, "*.index"))
	for _, ix := range idx {
		data, err := readSourceFile(ix)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			p := line
			if !filepath.IsAbs(p) {
				p = filepath.Join(datadir, p)
			}
			if _, err := os.Stat(p); err != nil {
				// Moved along with the datadir: try the same name next to the index.
				p = filepath.Join(filepath.Dir(ix), filepath.Base(line))
			}
			add(p)
		}
	}
	if len(out) == 0 {
		cands, _ := filepath.Glob(filepath.Join(datadir, "*.[0-9][0-9][0-9][0-9][0-9][0-9]"))
		sort.Strings(cands)
		for _, c := range cands {
			add(c)
		}
	}
	return out
}

func isBinlogFile(p string) bool {
	f, err := openSource(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var m [4]byte
	if _, err := io.ReadFull(f, m[:]); err != nil {
		return false
	}
	return string(m[:]) == string(binlogMagic)
}

// binlogXIDs collects every XID_EVENT's my_xid in the given binlogs.
// compressed counts transactions it couldn't look inside
// (binlog_transaction_compression).
func binlogXIDs(files []string) (xids map[uint64]bool, compressed int, err error) {
	xids = map[uint64]bool{}
	for _, p := range files {
		c, e := scanBinlogXIDs(p, xids)
		compressed += c
		if e != nil && err == nil {
			err = fmt.Errorf("%s: %w", filepath.Base(p), e)
		}
	}
	return xids, compressed, err
}

func scanBinlogXIDs(path string, xids map[uint64]bool) (compressed int, err error) {
	f, err := openSource(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var m [4]byte
	if _, err := io.ReadFull(r, m[:]); err != nil || string(m[:]) != string(binlogMagic) {
		return 0, errors.New("not a binary log (encrypted, or not a binlog at all)")
	}
	var hdr [binlogEventHeaderLen]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return compressed, nil // end of file (a torn last event is normal after a crash)
		}
		typ := hdr[4]
		size := int(binary.LittleEndian.Uint32(hdr[9:13]))
		if size < binlogEventHeaderLen {
			return compressed, fmt.Errorf("event with impossible size %d", size)
		}
		body := size - binlogEventHeaderLen
		switch typ {
		case binlogXIDEvent:
			if body < 8 {
				return compressed, errors.New("truncated XID event")
			}
			var x [8]byte
			if _, err := io.ReadFull(r, x[:]); err != nil {
				return compressed, nil
			}
			xids[binary.LittleEndian.Uint64(x[:])] = true
			body -= 8
		case binlogTransactionPayloadEvent:
			compressed++
		}
		if _, err := r.Discard(body); err != nil {
			return compressed, nil
		}
	}
}

// resolvePreparedFromBinlog commits (into db's in-memory WAL view) every
// prepared transaction the instance's binlog says committed, and reports
// what it did. Called once, right after opening db.
func resolvePreparedFromBinlog(db *rdbDB, datadir string) {
	if len(db.prepared) == 0 {
		return
	}
	files := findBinlogs(datadir)
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "%s RocksDB: %d transaction(s) were prepared in the WAL without a COMMIT marker, and no binary log was found in %s to tell whether they committed - left out (copy the binlog files and their .index next to the datadir to resolve them as the server would)\n",
			warn("warning:"), len(db.prepared), datadir)
		return
	}
	xids, compressed, err := binlogXIDs(files)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s reading the binary log: %v\n", warn("warning:"), err)
	}
	committed, rolledBack := db.commitPrepared(func(name []byte) bool {
		n, ok := mysqlXIDNumber(name)
		return ok && xids[n]
	})
	if committed > 0 && *verbose {
		fmt.Fprintf(os.Stderr, "%s RocksDB: %d prepared transaction(s) without a COMMIT marker in the WAL were found committed in the binary log - included, as crash recovery would\n",
			dim("note:"), committed)
	}
	if rolledBack > 0 {
		extra := ""
		if compressed > 0 {
			extra = fmt.Sprintf(" (the binlog also holds %d compressed transaction(s), binlog_transaction_compression, which weren't checked)", compressed)
		}
		fmt.Fprintf(os.Stderr, "%s RocksDB: %d prepared transaction(s) have no XID event in the binary log - rolled back, as crash recovery would%s\n",
			warn("warning:"), rolledBack, extra)
	}
}
