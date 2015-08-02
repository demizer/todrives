package core

import (
	"crypto/sha1"
	"encoding/hex"
	"hash"
	"io"
	"time"
)

type IoReaderWriter struct {
	io.Reader
	io.Writer
	size              uint64
	totalBytesWritten uint64
	totalRead         uint64
	timeStart         time.Time
	progress          copyProgress
	sha1              hash.Hash
}

func NewIoReaderWriter(outFile io.Writer, outFileSize uint64) *IoReaderWriter {
	i := &IoReaderWriter{
		Writer:    outFile,
		size:      outFileSize,
		timeStart: time.Now(),
		sha1:      sha1.New(),
	}
	return i
}

func (i *IoReaderWriter) MultiWriter() io.Writer {
	return io.MultiWriter(i, i.sha1)
}

type progressPoint struct {
	time              time.Time
	totalBytesWritten uint64
}

type copyProgress []progressPoint

func (c *copyProgress) addPoint(totalBytesWritten uint64) {
	*c = append(*c, progressPoint{
		time:              time.Now(),
		totalBytesWritten: totalBytesWritten,
	})
}

func (c *copyProgress) lastPoint() progressPoint {
	// if len(*c) == 0 {
	// return (*c)[0]
	// }
	return (*c)[len(*c)-1]
}

// Write writes to the io.Writer and also create a progress point for tracking
// write speed.
func (i *IoReaderWriter) Write(p []byte) (int, error) {
	n, err := i.Writer.Write(p)
	i.totalBytesWritten += uint64(n)
	var addPoint bool
	if err == nil {
		if len(i.progress) == 0 {
			addPoint = true
		} else if (time.Since(i.progress.lastPoint().time).Seconds()) < 1 {
			addPoint = true
		}
		if addPoint {
			i.progress.addPoint(i.totalBytesWritten)
		}
	}
	return n, err
}

func (i *IoReaderWriter) WriteBytesPerSecond() uint64 {
	if i.totalBytesWritten != i.size {
		return uint64(float64(i.totalBytesWritten) / time.Since(i.timeStart).Seconds())
	}
	return uint64(float64(i.size) / time.Since(i.timeStart).Seconds())
}

func (i *IoReaderWriter) Sha1SumToString() string {
	return hex.EncodeToString(i.sha1.Sum(nil))
}
