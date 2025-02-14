/*
 * MinIO Cloud Storage, (C) 2020 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package simdcsv

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"math/bits"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// Below is the same interface definition from encoding/csv

// A Reader reads records from a CSV-encoded file.
//
// As returned by NewReader, a Reader expects input conforming to RFC 4180.
// The exported fields can be changed to customize the details before the
// first call to Read or ReadAll.
//
// The Reader converts all \r\n sequences in its input to plain \n,
// including in multiline field values, so that the returned data does
// not depend on which line-ending convention an input file uses.
type Reader struct {
	sync.Mutex
	// Comma is the field delimiter.
	// It is set to comma (',') by NewReader.
	// Comma must be a valid rune and must not be \r, \n,
	// or the Unicode replacement character (0xFFFD).
	Comma rune

	// Comment, if not 0, is the comment character. Lines beginning with the
	// Comment character without preceding whitespace are ignored.
	// With leading whitespace the Comment character becomes part of the
	// field, even if TrimLeadingSpace is true.
	// Comment must be a valid rune and must not be \r, \n,
	// or the Unicode replacement character (0xFFFD).
	// It must also not be equal to Comma.
	Comment rune

	// FieldsPerRecord is the number of expected fields per record.
	// If FieldsPerRecord is positive, Read requires each record to
	// have the given number of fields. If FieldsPerRecord is 0, Read sets it to
	// the number of fields in the first record, so that future records must
	// have the same field count. If FieldsPerRecord is negative, no check is
	// made and records may have a variable number of fields.
	FieldsPerRecord int

	// If LazyQuotes is true, a quote may appear in an unquoted field and a
	// non-doubled quote may appear in a quoted field.
	LazyQuotes bool

	// If TrimLeadingSpace is true, leading white space in a field is ignored.
	// This is done even if the field delimiter, Comma, is white space.
	TrimLeadingSpace bool

	ReuseRecord   bool // Deprecated: Unused by simdcsv.
	TrailingComma bool // Deprecated: No longer used.

	r    *bufio.Reader
	rCsv *csv.Reader // Used as fallback when simd isn't supported

	//* state: IsStreaming when true, the readallstreaming process is active
	IsStreaming bool
	records     [][]string            //Current block of records
	hash        map[int]recordsOutput // seqences of blocks waiting
	currrecord  int                   //current record in block
	sequence    int                   // current block sequence number
	readchan    chan recordsOutput
}

var errInvalidDelim = errors.New("csv: invalid field or comment delimiter")

func validDelim(r rune) bool {
	return r != 0 && r != '"' && r != '\r' && r != '\n' && utf8.ValidRune(r) && r != utf8.RuneError
}

// NewReader returns a new Reader that reads from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{
		Comma: ',',
		r:     bufio.NewReader(r),
	}
}

type chunkInfo struct {
	sequence int
	chunk    []byte
	masks    []uint64
	postProc []uint64
	header   uint64
	trailer  uint64
	splitRow []byte
}

type recordsOutput struct {
	sequence int
	records  [][]string
	err      error
}

type chunkIn struct {
	buf  []byte
	last bool
}

// readAllStreaming reads all the remaining records from r.
func (r *Reader) readAllStreaming() (out chan recordsOutput) {

	if r.IsStreaming {
		return nil // We don't want 2 active readers
	}
	r.IsStreaming = true
	out = make(chan recordsOutput, 128)

	fallback := func(ioReader io.Reader) recordsOutput {
		rCsv := csv.NewReader(ioReader)
		rCsv.LazyQuotes = r.LazyQuotes
		rCsv.TrimLeadingSpace = r.TrimLeadingSpace
		rCsv.Comment = r.Comment
		rCsv.Comma = r.Comma
		rCsv.FieldsPerRecord = r.FieldsPerRecord
		rCsv.ReuseRecord = r.ReuseRecord
		rcds, err := rCsv.ReadAll()
		return recordsOutput{-1, rcds, err}
	}

	if r.Comma == r.Comment || !validDelim(r.Comma) || (r.Comment != 0 && !validDelim(r.Comment)) {
		go func() {
			out <- recordsOutput{-1, nil, errInvalidDelim}
			close(out)
		}()
		r.IsStreaming = false
		return
	}

	if r.LazyQuotes ||
		r.Comma != 0 && r.Comma > unicode.MaxLatin1 ||
		r.Comment != 0 && r.Comment > unicode.MaxLatin1 {
		go func() {
			out <- fallback(r.r)
			close(out)
		}()
		r.IsStreaming = false
		return
	}

	chunkSize := 320000

	// chunkSize must be a multiple of 64 bytes
	chunkSize = (chunkSize + 63) &^ 63
	masksSize := ((chunkSize >> 6) + 2) * 3 // add 2 extra slots as safety for masks

	// channel with slices of input
	bufchan := make(chan chunkIn, cap(out))

	go func() {

		defer func() {
			close(bufchan)
			r.IsStreaming = false
		}()

		br := bufio.NewReader(r.r)
		chunk := make([]byte, chunkSize)

		n, err := br.Read(chunk)
		if err == io.EOF {
			return
		} else if err != nil {
			log.Printf("bufio.Read() encounterend error: %v", err)
			return
		} else {
			chunk = chunk[:n]
		}

		for {
			chunkNext := make([]byte, chunkSize)

			n, err := br.Read(chunkNext)
			if err == io.EOF {
				if n > 0 {
					panic("last buffer should be empty")
				}
				bufchan <- chunkIn{chunk, true}
				break
			} else if err != nil {
				log.Printf("bufio.Read() encounterend error: %v", err)
				bufchan <- chunkIn{chunk, true}
				break
			} else {
				bufchan <- chunkIn{chunk, false}
				chunk = chunkNext[:n]
			}
		}
	}()

	// channel with preprocessed chunks
	chunks := make(chan chunkInfo, cap(out))

	go r.stage1Streaming(bufchan, chunkSize, masksSize, chunks)

	go func() {
		var wg sync.WaitGroup

		// Determine how many second stages to run in parallel
		const cores = 2
		wg.Add(cores)
		fieldsPerRecord := int64(r.FieldsPerRecord)

		for parallel := 0; parallel < cores; parallel++ {
			go r.stage2Streaming(chunks, &wg, &fieldsPerRecord, fallback, out)
		}

		wg.Wait()
		close(out)
	}()

	return
}

func (r *Reader) stage1Streaming(bufchan chan chunkIn, chunkSize int, masksSize int, chunks chan chunkInfo) {

	defer close(chunks)

	sequence := 0
	quoted := uint64(0) // initialized quoted state to unquoted

	splitRow := make([]byte, 0, 256)

	for chunk := range bufchan {

		postProcStream := make([]uint64, 0, ((chunkSize>>6)+1)*2)
		masksStream := make([]uint64, masksSize)

		masksStream, postProcStream, quoted = stage1PreprocessBufferEx(chunk.buf, uint64(r.Comma), quoted, &masksStream, &postProcStream)

		header, trailer := uint64(0), uint64(0)

		if sequence > 0 {
			for index := 0; index < len(masksStream); index += 3 {
				hr := bits.TrailingZeros64(masksStream[index])
				header += uint64(hr)
				if hr < 64 {
					// upon finding the first delimiter bit, we can break out
					// (since any adjacent delimiter bits, whether representing a newline or a carriage return,
					//  are treated as empty lines anyways)
					break
				}
			}
			if header == uint64(len(masksStream))/3*64 {
				// we have not found a newline delimiter, so set
				// header to size of chunk (meaning we will be skipping simd processing)
				header = uint64(len(chunk.buf))
			}
		}

		if !chunk.last && header < uint64(len(chunk.buf)) {
			for index := 3; index < len(masksStream); index += 3 {
				tr := bits.LeadingZeros64(masksStream[len(masksStream)-index])
				trailer += uint64(tr)
				if tr < 64 {
					break
				}
			}
		}

		splitRow = append(splitRow, chunk.buf[:header]...)

		if header < uint64(len(chunk.buf)) {
			chunks <- chunkInfo{sequence, chunk.buf, masksStream, postProcStream, header, trailer, splitRow}
		} else {
			chunks <- chunkInfo{sequence, nil, nil, nil, 0, 0, splitRow}
		}

		splitRow = make([]byte, 0, len(splitRow)*3/2)
		splitRow = append(splitRow, chunk.buf[len(chunk.buf)-int(trailer):]...)

		sequence++
	}
}

func (r *Reader) stage2Streaming(chunks chan chunkInfo, wg *sync.WaitGroup, fieldsPerRecord *int64, fallback func(ioReader io.Reader) recordsOutput, out chan recordsOutput) {
	defer wg.Done()

	simdlines, rowsSize, columnsSize := 1024, 500, 50000

	for chunkInfo := range chunks {

		simdrecords := make([][]string, 0, simdlines)

		rows := make([]uint64, rowsSize, rowsSize)
		columns := make([]string, columnsSize, columnsSize)
		inputStage2, outputStage2 := newInputStage2(), outputAsm{}

		skipRowsForPostProcessing := 0
		if len(chunkInfo.splitRow) > 0 { // first append the row split between chunks
			records, err := encodingCsv(chunkInfo.splitRow, r.Comma)
			if err != nil {
				out <- recordsOutput{-1, nil, err}
				break
			}
			simdrecords = append(simdrecords, records...)
			skipRowsForPostProcessing = len(simdrecords)
		}

		if chunkInfo.chunk != nil {

			outputStage2.strData = chunkInfo.header & 0x3f // reinit strData for every chunk (fields do not span chunks)

			skip := chunkInfo.header >> 6
			shift := chunkInfo.header & 0x3f

			chunkInfo.masks[skip*3+0] &= ^uint64((1 << shift) - 1)
			chunkInfo.masks[skip*3+1] &= ^uint64((1 << shift) - 1)
			chunkInfo.masks[skip*3+2] &= ^uint64((1 << shift) - 1)

			skipTz := (chunkInfo.trailer >> 6) + 1
			shiftTz := chunkInfo.trailer & 0x3f

			chunkInfo.masks[len(chunkInfo.masks)-int(skipTz)*3+0] <<= shiftTz
			chunkInfo.masks[len(chunkInfo.masks)-int(skipTz)*3+1] <<= shiftTz
			chunkInfo.masks[len(chunkInfo.masks)-int(skipTz)*3+2] <<= shiftTz
			chunkInfo.masks[len(chunkInfo.masks)-int(skipTz)*3+0] >>= shiftTz
			chunkInfo.masks[len(chunkInfo.masks)-int(skipTz)*3+1] >>= shiftTz
			chunkInfo.masks[len(chunkInfo.masks)-int(skipTz)*3+2] >>= shiftTz

			var parsingError bool
			rows, columns, parsingError = stage2ParseBufferExStreaming(chunkInfo.chunk[skip*0x40:len(chunkInfo.chunk)-int(chunkInfo.trailer)], chunkInfo.masks[skip*3:], '\n', &inputStage2, &outputStage2, &rows, &columns)
			if parsingError {
				out <- fallback(bytes.NewReader(chunkInfo.chunk[skip*0x40 : len(chunkInfo.chunk)-int(chunkInfo.trailer)]))
				break
			}

			for line := 0; line < outputStage2.line; line += 2 {
				simdrecords = append(simdrecords, columns[rows[line]:rows[line]+rows[line+1]])
			}

			columns = columns[:(outputStage2.index)/2]
			rows = rows[:outputStage2.line]

			if len(chunkInfo.postProc) > 0 {
				pprs := getPostProcRows(chunkInfo.chunk, chunkInfo.postProc, simdrecords[skipRowsForPostProcessing:])
				for _, ppr := range pprs {
					for r := ppr.start + skipRowsForPostProcessing; r < ppr.end+skipRowsForPostProcessing; r++ {
						for c := range simdrecords[r] {
							simdrecords[r][c] = strings.ReplaceAll(simdrecords[r][c], "\"\"", "\"")
							simdrecords[r][c] = strings.ReplaceAll(simdrecords[r][c], "\r\n", "\n")
						}
					}
				}
			}

			if errSimd := ensureFieldsPerRecord(&simdrecords, fieldsPerRecord); errSimd != nil {
				out <- fallback(bytes.NewReader(chunkInfo.chunk[skip*0x40 : len(chunkInfo.chunk)-int(chunkInfo.trailer)]))
				break
			}
		}

		if r.Comment != 0 {
			filterOutComments(&simdrecords, byte(r.Comment))
		}
		if r.TrimLeadingSpace {
			trimLeadingSpace(&simdrecords)
		}

		if simdlines < len(simdrecords) {
			simdlines = len(simdrecords) * 9 >> 3
		}
		if rowsSize < cap(rows) {
			rowsSize = cap(rows) * 3 / 4
		}
		if columnsSize < cap(columns) {
			columnsSize = cap(columns) * 3 / 4
		}

		out <- recordsOutput{chunkInfo.sequence, simdrecords, nil}
	}
}

// ReadAll reads all the remaining records from r.
// Each record is a slice of fields.
// A successful call returns err == nil, not err == io.EOF. Because ReadAll is
// defined to read until EOF, it does not treat end of file as an error to be
// reported.
func (r *Reader) ReadAll() ([][]string, error) {
	r.Lock()
	defer r.Unlock()
	if !SupportedCPU() {
		if r.rCsv == nil {
			r.rCsv = csv.NewReader(r.r)
			defer func() {
				r.rCsv = nil
			}()
			r.rCsv.LazyQuotes = r.LazyQuotes
			r.rCsv.TrimLeadingSpace = r.TrimLeadingSpace
			r.rCsv.Comment = r.Comment
			r.rCsv.Comma = r.Comma
			r.rCsv.FieldsPerRecord = r.FieldsPerRecord
			r.rCsv.ReuseRecord = r.ReuseRecord
		}

		return r.rCsv.ReadAll()
	}

	out := r.readAllStreaming()

	records := make([][]string, 0)
	hash := make(map[int][][]string)
	sequence := 0

	for rcrds := range out {
		if rcrds.err != nil {
			// upon encountering an error ...
			for range out {
				// ... drain channel
			}
			return nil, rcrds.err
		}

		// check whether number is in sequence
		if rcrds.sequence > sequence {
			hash[rcrds.sequence] = rcrds.records
			continue
		}

		records = append(records, rcrds.records...)
		sequence++

		// check if we already received higher sequence numbers
		for {
			if val, ok := hash[sequence]; ok {
				records = append(records, val...)
				delete(hash, sequence)
				sequence++
			} else {
				break
			}
		}
	}

	if len(records) == 0 {
		return nil, nil
	} else {
		return records, nil
	}
}

func (r *Reader) Read() ([]string, error) {
	r.Lock()
	defer r.Unlock()
	if !SupportedCPU() {
		if r.rCsv == nil {
			r.rCsv = csv.NewReader(r.r)
			r.rCsv.LazyQuotes = r.LazyQuotes
			r.rCsv.TrimLeadingSpace = r.TrimLeadingSpace
			r.rCsv.Comment = r.Comment
			r.rCsv.Comma = r.Comma
			r.rCsv.FieldsPerRecord = r.FieldsPerRecord
			r.rCsv.ReuseRecord = r.ReuseRecord
		}

		return r.rCsv.Read()
	}

	if !r.IsStreaming {
		r.records = make([][]string, 0)
		r.hash = make(map[int]recordsOutput)
		r.currrecord = 0
		r.sequence = 0
		r.readchan = r.readAllStreaming()
	}

	if r.currrecord >= len(r.records) {
		err := r.nextblock()
		if err != nil {
			return nil, err
		}
	}
	ret := r.records[r.currrecord]
	r.currrecord++
	return ret, nil

}

func (r *Reader) clearchan() {
	for _ = range r.readchan {
		//...
	}
}

func (r *Reader) nextblock() error {

	for {
		rcrds, ok := r.hash[r.sequence]
		if ok {
			r.sequence++
			if len(rcrds.records) == 0 {
				continue
			}
			if rcrds.err != nil {
				r.clearchan()
				return rcrds.err
			}
			r.currrecord = 0
			r.records = rcrds.records
			return nil
		}
		break
	}
	for rcrds := range r.readchan {
		if rcrds.sequence > r.sequence {
			r.hash[rcrds.sequence] = rcrds
			continue
		}
		if rcrds.sequence == r.sequence {
			delete(r.hash, rcrds.sequence)
			r.sequence++
			if rcrds.err != nil {
				r.clearchan()
				return rcrds.err
			}
			if len(rcrds.records) == 0 {
				continue
			}
			r.records = rcrds.records
			r.currrecord = 0
			return nil
		}
	}
	return io.EOF
}

func filterOutComments(records *[][]string, comment byte) {

	// iterate in reverse so as to prevent starting over when removing element
	for i := len(*records) - 1; i >= 0; i-- {
		record := (*records)[i]
		if len(record) > 0 && len(record[0]) > 0 && record[0][0] == comment {
			*records = append((*records)[:i], (*records)[i+1:len(*records)]...)
		}
	}
}

func ensureFieldsPerRecord(records *[][]string, fieldsPerRecord *int64) error {

	if atomic.LoadInt64(fieldsPerRecord) == 0 {
		if len(*records) > 0 {
			atomic.StoreInt64(fieldsPerRecord, int64(len((*records)[0])))
		}
	}

	fpr := atomic.LoadInt64(fieldsPerRecord)
	if fpr > 0 {
		for i, record := range *records {
			if int64(len(record)) != fpr {
				*records = nil
				return errors.New(fmt.Sprintf("record on line %d: wrong number of fields", i+1))
			}
		}
	}
	return nil
}

func trimLeadingSpace(records *[][]string) {

	for i := 0; i < len(*records); i++ {
		for j := range (*records)[i] {
			(*records)[i][j] = strings.TrimLeftFunc((*records)[i][j], func(r rune) bool {
				return unicode.IsSpace(r)
			})
		}
	}
}

func allocMasks(buf []byte) []uint64 {
	return make([]uint64, ((len(buf)>>6)+4)*3)
}

func encodingCsv(csvData []byte, sep rune) ([][]string, error) {
	r := csv.NewReader(bytes.NewReader(csvData))
	r.Comma = sep
	return r.ReadAll()
}
