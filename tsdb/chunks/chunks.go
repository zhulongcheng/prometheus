// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package chunks

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
	"github.com/prometheus/prometheus/tsdb/fileutil"
)

// Segment header fields constants.
const (
	// MagicChunks is 4 bytes at the head of a series file.
	MagicChunks = 0x85BD40DD
	// MagicChunksSize is the size in bytes of MagicChunks.
	MagicChunksSize          = 4
	chunksFormatV1           = 1
	ChunksFormatVersionSize  = 1
	segmentHeaderPaddingSize = 3
	// SegmentHeaderSize defines the total size of the header part.
	SegmentHeaderSize = MagicChunksSize + ChunksFormatVersionSize + segmentHeaderPaddingSize
)

// Chunk fields constants.
const (
	// MaxChunkLengthFieldSize defines the maximum size of the data length part.
	MaxChunkLengthFieldSize = binary.MaxVarintLen32
	// ChunkEncodingSize defines the size of the chunk encoding part.
	ChunkEncodingSize = 1
)

// Meta holds information about a chunk of data.
type Meta struct {
	// Ref and Chunk hold either a reference that can be used to retrieve
	// chunk data or the data itself.
	// When it is a reference it is the segment offset at which the chunk bytes start.
	// Generally, only one of them is set.
	Ref   uint64
	Chunk chunkenc.Chunk

	// Time range the data covers.
	// When MaxTime == math.MaxInt64 the chunk is still open and being appended to.
	MinTime, MaxTime int64
}

// writeHash writes the chunk encoding and raw data into the provided hash.
func (cm *Meta) writeHash(h hash.Hash, buf []byte) error {
	buf = append(buf[:0], byte(cm.Chunk.Encoding()))
	if _, err := h.Write(buf[:1]); err != nil {
		return err
	}
	if _, err := h.Write(cm.Chunk.Bytes()); err != nil {
		return err
	}
	return nil
}

// OverlapsClosedInterval Returns true if the chunk overlaps [mint, maxt].
func (cm *Meta) OverlapsClosedInterval(mint, maxt int64) bool {
	// The chunk itself is a closed interval [cm.MinTime, cm.MaxTime].
	return cm.MinTime <= maxt && mint <= cm.MaxTime
}

var (
	errInvalidSize = fmt.Errorf("invalid size")
)

var castagnoliTable *crc32.Table

func init() {
	castagnoliTable = crc32.MakeTable(crc32.Castagnoli)
}

// newCRC32 initializes a CRC32 hash with a preconfigured polynomial, so the
// polynomial may be easily changed in one location at a later time, if necessary.
func newCRC32() hash.Hash32 {
	return crc32.New(castagnoliTable)
}

// Writer implements the ChunkWriter interface for the standard
// serialization format.
type Writer struct {
	dirFile *os.File
	files   []*os.File
	wbuf    *bufio.Writer
	n       int64
	crc32   hash.Hash
	buf     [binary.MaxVarintLen32]byte

	segmentSize int64
}

const (
	// DefaultChunkSegmentSize is the default chunks segment size.
	DefaultChunkSegmentSize = 512 * 1024 * 1024
)

// NewWriter returns a new writer against the given directory.
// When the segment size argument is less than 1 it uses the DefaultChunkSegmentSize.
func NewWriter(dir string, segmentSize int64) (*Writer, error) {
	if segmentSize <= 0 {
		segmentSize = DefaultChunkSegmentSize
	}
	if err := os.MkdirAll(dir, 0777); err != nil {
		return nil, err
	}
	dirFile, err := fileutil.OpenDir(dir)
	if err != nil {
		return nil, err
	}
	cw := &Writer{
		dirFile:     dirFile,
		n:           0,
		crc32:       newCRC32(),
		segmentSize: segmentSize,
	}
	return cw, nil
}

func (w *Writer) tail() *os.File {
	if len(w.files) == 0 {
		return nil
	}
	return w.files[len(w.files)-1]
}

// finalizeTail writes all pending data to the current tail file,
// truncates its size, and closes it.
func (w *Writer) finalizeTail() error {
	tf := w.tail()
	if tf == nil {
		return nil
	}

	if err := w.wbuf.Flush(); err != nil {
		return err
	}
	if err := tf.Sync(); err != nil {
		return err
	}
	// As the file was pre-allocated, we truncate any superfluous zero bytes.
	off, err := tf.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if err := tf.Truncate(off); err != nil {
		return err
	}

	return tf.Close()
}

func (w *Writer) cut() error {
	// Sync current tail to disk and close.
	if err := w.finalizeTail(); err != nil {
		return err
	}

	p, _, err := nextSequenceFile(w.dirFile.Name())
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	if err = fileutil.Preallocate(f, w.segmentSize, true); err != nil {
		return err
	}
	if err = w.dirFile.Sync(); err != nil {
		return err
	}

	// Write header metadata for new file.
	metab := make([]byte, SegmentHeaderSize)
	binary.BigEndian.PutUint32(metab[:MagicChunksSize], MagicChunks)
	metab[4] = chunksFormatV1

	n, err := f.Write(metab)
	if err != nil {
		return err
	}
	w.n = int64(n)

	w.files = append(w.files, f)
	if w.wbuf != nil {
		w.wbuf.Reset(f)
	} else {
		w.wbuf = bufio.NewWriterSize(f, 8*1024*1024)
	}

	return nil
}

func (w *Writer) write(b []byte) error {
	n, err := w.wbuf.Write(b)
	w.n += int64(n)
	return err
}

// MergeOverlappingChunks removes the samples whose timestamp is overlapping.
// The last appearing sample is retained in case there is overlapping.
// This assumes that `chks []Meta` is sorted w.r.t. MinTime.
func MergeOverlappingChunks(chks []Meta) ([]Meta, error) {
	if len(chks) < 2 {
		return chks, nil
	}
	newChks := make([]Meta, 0, len(chks)) // Will contain the merged chunks.
	newChks = append(newChks, chks[0])
	last := 0
	for _, c := range chks[1:] {
		// We need to check only the last chunk in newChks.
		// Reason: (1) newChks[last-1].MaxTime < newChks[last].MinTime (non overlapping)
		//         (2) As chks are sorted w.r.t. MinTime, newChks[last].MinTime < c.MinTime.
		// So never overlaps with newChks[last-1] or anything before that.
		if c.MinTime > newChks[last].MaxTime {
			newChks = append(newChks, c)
			last++
			continue
		}
		nc := &newChks[last]
		if c.MaxTime > nc.MaxTime {
			nc.MaxTime = c.MaxTime
		}
		chk, err := MergeChunks(nc.Chunk, c.Chunk)
		if err != nil {
			return nil, err
		}
		nc.Chunk = chk
	}

	return newChks, nil
}

// MergeChunks vertically merges a and b, i.e., if there is any sample
// with same timestamp in both a and b, the sample in a is discarded.
func MergeChunks(a, b chunkenc.Chunk) (*chunkenc.XORChunk, error) {
	newChunk := chunkenc.NewXORChunk()
	app, err := newChunk.Appender()
	if err != nil {
		return nil, err
	}
	ait := a.Iterator(nil)
	bit := b.Iterator(nil)
	aok, bok := ait.Next(), bit.Next()
	for aok && bok {
		at, av := ait.At()
		bt, bv := bit.At()
		if at < bt {
			app.Append(at, av)
			aok = ait.Next()
		} else if bt < at {
			app.Append(bt, bv)
			bok = bit.Next()
		} else {
			app.Append(bt, bv)
			aok = ait.Next()
			bok = bit.Next()
		}
	}
	for aok {
		at, av := ait.At()
		app.Append(at, av)
		aok = ait.Next()
	}
	for bok {
		bt, bv := bit.At()
		app.Append(bt, bv)
		bok = bit.Next()
	}
	if ait.Err() != nil {
		return nil, ait.Err()
	}
	if bit.Err() != nil {
		return nil, bit.Err()
	}
	return newChunk, nil
}

// WriteChunks writes as many chunks as possible to the current segment,
// cuts a new segment when the current segment is full and
// writes the rest of the chunks in the new segment.
func (w *Writer) WriteChunks(chks ...Meta) error {
	var (
		chksBatchSize int64
		end           int
	)

	// w.wbuf == nil means it is the first chunk
	// so need to start a new segment.
	if w.wbuf == nil {
		if err := w.cut(); err != nil {
			return err
		}
	}

	for _, chk := range chks {
		// Each chunk contains: data length + encoding + the data itself + crc32
		chksBatchSize += int64(MaxChunkLengthFieldSize) // The data length is a variable length field so use the maximum possible value.
		chksBatchSize += ChunkEncodingSize              // The chunk encoding.
		chksBatchSize += int64(len(chk.Chunk.Bytes()))  // The data itself.
		chksBatchSize += crc32.Size                     // The 4 bytes of crc32

		end++
		if chksBatchSize+w.n > w.segmentSize {
			if end > 1 {
				// Don't include the last chunk only if there are >1 chunks.
				// This will keep segment size within the configured limit.
				// If a single chunks is bigger than the configured limit,
				// we cannot do much.
				end--
			}
			if err := w.writeChunks(chks[:end]); err != nil {
				return err
			}
			chks = chks[end:]
			chksBatchSize = 0
			end = 0
			// Cut a new segment only when there are more chunks to write.
			// This avoids creating a new empty segment.
			if len(chks) > 0 {
				if err := w.cut(); err != nil {
					return err
				}
			}
		}
	}

	return w.writeChunks(chks)
}

// writeChunks writes the chunks into the current segment irrespective
// of the configured segment size limit. A segment should have been already
// started before calling this.
func (w *Writer) writeChunks(chks []Meta) error {
	if len(chks) == 0 {
		return nil
	}

	var seq = uint64(w.seq()) << 32
	for i := range chks {
		chk := &chks[i]

		// The reference is set to the segment index and the offset where
		// the data starts for this chunk.
		//
		// The upper 4 bytes are for the segment index and
		// The lower 4 bytes are for the segment offset where to start reading this chunk.
		chk.Ref = seq | uint64(w.n)

		n := binary.PutUvarint(w.buf[:], uint64(len(chk.Chunk.Bytes())))

		if err := w.write(w.buf[:n]); err != nil {
			return err
		}
		w.buf[0] = byte(chk.Chunk.Encoding())
		if err := w.write(w.buf[:1]); err != nil {
			return err
		}
		if err := w.write(chk.Chunk.Bytes()); err != nil {
			return err
		}

		w.crc32.Reset()
		if err := chk.writeHash(w.crc32, w.buf[:]); err != nil {
			return err
		}
		if err := w.write(w.crc32.Sum(w.buf[:0])); err != nil {
			return err
		}
	}

	return nil
}

func (w *Writer) seq() int {
	return len(w.files) - 1
}

func (w *Writer) Close() error {
	if err := w.finalizeTail(); err != nil {
		return err
	}

	// close dir file (if not windows platform will fail on rename)
	return w.dirFile.Close()
}

// ByteSlice abstracts a byte slice.
type ByteSlice interface {
	Len() int
	Range(start, end int) []byte
}

type realByteSlice []byte

func (b realByteSlice) Len() int {
	return len(b)
}

func (b realByteSlice) Range(start, end int) []byte {
	return b[start:end]
}

func (b realByteSlice) Sub(start, end int) ByteSlice {
	return b[start:end]
}

// Reader implements a ChunkReader for a serialized byte stream
// of series data.
type Reader struct {
	// The underlying bytes holding the encoded series data.
	// Each slice holds the data for a different segment.
	bs    []ByteSlice
	cs    []io.Closer // Closers for resources behind the byte slices.
	size  int64       // The total size of bytes in the reader.
	pool  chunkenc.Pool
	crc32 hash.Hash
	buf   [binary.MaxVarintLen32]byte
}

func newReader(bs []ByteSlice, cs []io.Closer, pool chunkenc.Pool) (*Reader, error) {
	cr := Reader{pool: pool, bs: bs, cs: cs, crc32: newCRC32()}
	var totalSize int64

	for i, b := range cr.bs {
		if b.Len() < SegmentHeaderSize {
			return nil, errors.Wrapf(errInvalidSize, "invalid segment header in segment %d", i)
		}
		// Verify magic number.
		if m := binary.BigEndian.Uint32(b.Range(0, MagicChunksSize)); m != MagicChunks {
			return nil, errors.Errorf("invalid magic number %x", m)
		}

		// Verify chunk format version.
		if v := int(b.Range(MagicChunksSize, MagicChunksSize+ChunksFormatVersionSize)[0]); v != chunksFormatV1 {
			return nil, errors.Errorf("invalid chunk format version %d", v)
		}
		totalSize += int64(b.Len())
	}
	cr.size = totalSize
	return &cr, nil
}

// NewDirReader returns a new Reader against sequentially numbered files in the
// given directory.
func NewDirReader(dir string, pool chunkenc.Pool) (*Reader, error) {
	files, err := sequenceFiles(dir)
	if err != nil {
		return nil, err
	}
	if pool == nil {
		pool = chunkenc.NewPool()
	}

	var (
		bs   []ByteSlice
		cs   []io.Closer
		merr tsdb_errors.MultiError
	)
	for _, fn := range files {
		f, err := fileutil.OpenMmapFile(fn)
		if err != nil {
			merr.Add(errors.Wrap(err, "mmap files"))
			merr.Add(closeAll(cs))
			return nil, merr
		}
		cs = append(cs, f)
		bs = append(bs, realByteSlice(f.Bytes()))
	}

	reader, err := newReader(bs, cs, pool)
	if err != nil {
		merr.Add(err)
		merr.Add(closeAll(cs))
		return nil, merr
	}
	return reader, nil
}

func (s *Reader) Close() error {
	return closeAll(s.cs)
}

// Size returns the size of the chunks.
func (s *Reader) Size() int64 {
	return s.size
}

// Chunk returns a chunk from a given reference.
func (s *Reader) Chunk(ref uint64) (chunkenc.Chunk, error) {
	var (
		// Get the upper 4 bytes.
		// These contain the segment index.
		sgmIndex = int(ref >> 32)
		// Get the lower 4 bytes.
		// These contain the segment offset where the data for this chunk starts.
		sgmChunkStart = int((ref << 32) >> 32)
	)

	if sgmIndex >= len(s.bs) {
		return nil, errors.Errorf("segment index %d out of range", sgmIndex)
	}

	sgmBytes := s.bs[sgmIndex]

	if sgmChunkStart+MaxChunkLengthFieldSize > sgmBytes.Len() {
		return nil, errors.Errorf("segment doesn't include enough bytes to read the chunk size data field - required:%v, available:%v", sgmChunkStart+MaxChunkLengthFieldSize, sgmBytes.Len())
	}
	// With the minimum chunk length this should never cause us reading
	// over the end of the slice.
	c := sgmBytes.Range(sgmChunkStart, sgmChunkStart+MaxChunkLengthFieldSize)
	chkDataLen, n := binary.Uvarint(c)
	if n <= 0 {
		return nil, errors.Errorf("reading chunk length failed with %d", n)
	}

	chkEncStart := sgmChunkStart + n
	chkEnd := chkEncStart + ChunkEncodingSize + int(chkDataLen) + crc32.Size
	chkDataStart := chkEncStart + ChunkEncodingSize
	chkDataEnd := chkEnd - crc32.Size

	if chkEnd > sgmBytes.Len() {
		return nil, errors.Errorf("segment doesn't include enough bytes to read the chunk - required:%v, available:%v", chkEnd, sgmBytes.Len())
	}

	sum := sgmBytes.Range(chkEnd-crc32.Size, chkEnd)
	s.crc32.Reset()
	if _, err := s.crc32.Write(sgmBytes.Range(chkEncStart, chkDataEnd)); err != nil {
		return nil, err
	}
	if act := s.crc32.Sum(s.buf[:0]); !bytes.Equal(act, sum) {
		return nil, errors.Errorf("unexpected checksum %x, expected %x", act, sum)
	}

	chkData := sgmBytes.Range(chkDataStart, chkDataEnd)
	chkEnc := sgmBytes.Range(chkEncStart, chkEncStart+ChunkEncodingSize)[0]
	return s.pool.Get(chunkenc.Encoding(chkEnc), chkData)
}

func nextSequenceFile(dir string) (string, int, error) {
	names, err := fileutil.ReadDir(dir)
	if err != nil {
		return "", 0, err
	}

	i := uint64(0)
	for _, n := range names {
		j, err := strconv.ParseUint(n, 10, 64)
		if err != nil {
			continue
		}
		i = j
	}
	return filepath.Join(dir, fmt.Sprintf("%0.6d", i+1)), int(i + 1), nil
}

func sequenceFiles(dir string) ([]string, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var res []string

	for _, fi := range files {
		if _, err := strconv.ParseUint(fi.Name(), 10, 64); err != nil {
			continue
		}
		res = append(res, filepath.Join(dir, fi.Name()))
	}
	return res, nil
}

func closeAll(cs []io.Closer) error {
	var merr tsdb_errors.MultiError

	for _, c := range cs {
		merr.Add(c.Close())
	}
	return merr.Err()
}
