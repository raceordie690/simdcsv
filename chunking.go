package simdcsv

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
)

type chunkInput struct {
	part  int
	chunk []byte
}

type chunkResult struct {
	part       int
	widowSize  uint64
	orphanSize uint64
	status     chunkStatus
}

const PREFIX_SIZE = 64 * 1024

func detectQoPattern(input []byte) bool {

	for i, q := range input[:len(input)-1] {
		if q == '"' {
			o := input[i+1]
			if o != '"' && o != ',' && o != '\n' {
				return true
			}
		}
	}
	return false
}

func detectOqPattern(input []byte) bool {

	for i, q := range input[1:] {
		if q == '"' {
			o := input[i]
			if o != '"' && o != ',' && o != '\n' {
				return true
			}
		}
	}
	return false
}

func determineAmbiguity(prefixChunk []byte) (ambiguous bool) {

	hasQo := detectQoPattern(prefixChunk)
	hasOq := detectOqPattern(prefixChunk)
	ambiguous = hasQo == false && hasOq == false

	return
}

type chunkStatus int

// Determination of start of first complete line was
// definitive or not
const (
	Unambigous chunkStatus = iota
	Ambigous
)

func (s chunkStatus) String() string {
	return [...]string{"Unambigous", "Ambigous"}[s]
}

func deriveChunkResult(in chunkInput) chunkResult {

	prefixSize := PREFIX_SIZE
	if len(in.chunk) < prefixSize {
		prefixSize = len(in.chunk)
	}

	chunkStatus := Unambigous
	if bytes.ContainsRune(in.chunk[:prefixSize], '"') {
		if determineAmbiguity(in.chunk[:prefixSize]) {
			chunkStatus = Ambigous
		}
	}

	widowSize := uint64(0)
	for i := 0; i < len(in.chunk); i++ {
		if in.chunk[i] == '\n' {
			break
		}
		widowSize++
	}

	orphanSize := uint64(0)
	for i := len(in.chunk) - 1; i >= 0; i-- {
		if in.chunk[i] == '\n' {
			break
		}
		orphanSize++
	}

	return chunkResult{in.part, widowSize, orphanSize, chunkStatus}
}

func chunkWorker(chunks <-chan chunkInput, results chan<- chunkResult) {

	for in := range chunks {
		results <- deriveChunkResult(in)
	}
}

func ChunkBlob(blob []byte, chunkSize uint64) {

	var wg sync.WaitGroup
	chunks := make(chan chunkInput)
	results := make(chan chunkResult)

	// Start one go routine per CPU
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chunkWorker(chunks, results)
		}()
	}

	// Push chunks onto input channel
	go func() {
		for part, start := 0, uint64(0); ; start += chunkSize {

			end := start + chunkSize
			if end > uint64(len(blob)) {
				end = uint64(len(blob))
			}

			chunks <- chunkInput{part, blob[start:end]}

			if end >= uint64(len(blob)) {
				break
			}

			part++
		}

		// Close input channel
		close(chunks)
	}()

	// Wait for workers to complete
	go func() {
		wg.Wait()
		close(results) // Close output channel
	}()

	for r := range results {
		fmt.Println(r, r.status.String())
	}
}
