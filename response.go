package grab

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Response represents the response to a completed or in-process download
// request.
//
// A response may be returned as soon a HTTP response is received from a remote
// server, but before the body content has started transferring.
//
// All Response method calls are thread-safe.
type Response struct {
	// The Request that was sent to obtain this Response.
	Request *Request

	// HTTPResponse specifies the HTTP response received from the remote server.
	//
	// The response Body should not be used as it will be consumed and closed by
	// grab.
	HTTPResponse *http.Response

	// Filename specifies the path where the file transfer is stored in local
	// storage.
	Filename string

	// Size specifies the total expected size of the file transfer.
	Size int64

	// Start specifies the time at which the file transfer started.
	Start time.Time

	// End specifies the time at which the file transfer completed.
	//
	// This should not be read until IsComplete returns true.
	End time.Time

	// CanResume specifies that the remote server advertised that it can resume
	// previous downloads as the 'Accept-Ranges: bytes' is set.
	CanResume bool

	// DidResume specifies that the file transfer resumed a previously
	// incomplete transfer.
	DidResume bool

	// Done is closed once the transfer is finalized, either successfully or with
	// errors.
	Done chan struct{}

	// ctx is a Context that controls cancellation of an inprogress transfer
	ctx context.Context

	// cancel is a cancel func that can be used to cancel the context of this
	// Response.
	cancel context.CancelFunc

	// writer is the file handle used to write the downloaded file to local
	// storage
	writer io.WriteCloser

	writeFlags int

	// bytesCompleted specifies the number of bytes which were already
	// transferred before this transfer began.
	bytesResumed int64

	// bytesTransferred specifies the number of bytes which have already been
	// transferred and should only be accessed atomically.
	bytesTransferred int64

	// bufferSize specifies the site in bytes of the transfer buffer.
	bufferSize int

	// Error specifies any error that may have occurred during the file
	// transfer.
	//
	// This should not be read until IsComplete returns true.
	err error

	// fi is the FileInfo for the destination file if it already existed before
	// transfer started.
	fi os.FileInfo
}

// Cancel cancels the file transfer by cancelling the underlying Context for
// this Response. Cancel blocks until the transfer is closed and returns any
// error, typically, context.Canceled.
func (c *Response) Cancel() error {
	c.cancel()
	return c.Err()
}

// Wait blocks until the underlying file transfer is completed. If the transfer
// is already completed, Wait returns immediately.
func (c *Response) Wait() {
	<-c.Done
}

// Err blocks the calling goroutine until the underlying file transfer is
// completed and returns any error that may have occurred or nil. If the
// transfer is already completed, Err returns immediately.
func (c *Response) Err() error {
	<-c.Done
	return c.err
}

// IsComplete indicates whether the Response transfer context has completed with
// either a success or failure. If the transfer was unsuccessful, Response.Error
// will be non-nil.
func (c *Response) IsComplete() bool {
	select {
	case <-c.Done:
		return true
	default:
		return false
	}
}

// BytesTransferred returns the number of bytes which have already been
// downloaded, including any data used to resume a previous download.
func (c *Response) BytesTransferred() int64 {
	return atomic.LoadInt64(&c.bytesTransferred)
}

// Progress returns the ratio of bytes which have already been downloaded over
// the total file size as a fraction of 1.00.
//
// Multiply the returned value by 100 to return the percentage completed.
func (c *Response) Progress() float64 {
	if c.Size == 0 {
		return 0
	}

	return float64(c.BytesTransferred()) / float64(c.Size)
}

// Duration returns the duration of a file transfer. If the transfer is in
// process, the duration will be between now and the start of the transfer. If
// the transfer is complete, the duration will be between the start and end of
// the completed transfer process.
func (c *Response) Duration() time.Duration {
	if c.IsComplete() {
		return c.End.Sub(c.Start)
	}

	return time.Now().Sub(c.Start)
}

// ETA returns the estimated time at which the the download will complete. If
// the transfer has already complete, the actual end time will be returned.
func (c *Response) ETA() time.Time {
	if c.IsComplete() {
		return c.End
	}

	// total progress through transfer
	transferred := c.BytesTransferred()
	if transferred == 0 {
		return time.Time{}
	}

	// bytes remaining
	remainder := c.Size - transferred

	// time elapsed
	duration := time.Now().Sub(c.Start)

	// average bytes per second for transfer
	bps := float64(transferred-c.bytesResumed) / duration.Seconds()

	// estimated seconds remaining
	secs := float64(remainder) / bps

	return time.Now().Add(time.Duration(secs) * time.Second)
}

// AverageBytesPerSecond returns the average bytes transferred per second over
// the duration of the file transfer.
func (c *Response) AverageBytesPerSecond() float64 {
	return float64(c.BytesTransferred()-c.bytesResumed) / c.Duration().Seconds()
}

// setFileInfo sets Response.fi for the given Response.Filename. nil is set if
// the file does not exist or is a directory.
func (c *Response) setFileInfo() error {
	fi, err := os.Stat(c.Filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	if fi.IsDir() {
		c.Filename = ""
		return nil
	}

	c.fi = fi

	return nil
}

// readResponse reads a http.Response and updates Response.HTTPResponse,
// Response.Size, Response.Filename and Response.CanResume. An error is returned
// if any information returned from the remote server mismatches what is
// expected for the associated Request.
func (c *Response) readResponse(resp *http.Response) error {
	c.HTTPResponse = resp
	c.Size = c.bytesTransferred + resp.ContentLength

	if resp.Header.Get("Accept-Ranges") == "bytes" {
		c.CanResume = true
	}

	// check expected size
	if resp.ContentLength > 0 {
		if c.Request.Size > 0 && c.Request.Size != c.Size {
			return ErrBadLength
		}
		if c.fi != nil && c.fi.Size() > c.Size {
			return ErrBadLength
		}
	}

	// check filename
	if c.Filename == "" {
		filename, err := guessFilename(resp)
		if err != nil {
			return err
		}

		// Request.Filename will be empty or a directory
		c.Filename = filepath.Join(c.Request.Filename, filename)
		if err := c.setFileInfo(); err != nil {
			return err
		}
	}

	return nil
}

// checkExisting returns true if a file already exists for this request and is
// 100% completed. The size of the file is checked against Request.Size if set,
// or the Content-Length returned by the remote server.
//
// If a checksum has been requested, it will be executed on the existing file
// and an error returned if it fails validation.
//
// TODO: check timestamps and/or E-Tags
func (c *Response) checkExisting() (bool, error) {
	if c.fi == nil {
		return false, nil
	}

	if c.Request.SkipExisting {
		return true, ErrFileExists
	}

	// determine expected file size
	size := c.Request.Size
	if size == 0 && c.HTTPResponse != nil {
		// This assumes that the HTTPResponse is for a HEAD or non-ranged GET
		// request. Ranged requests will not return the full file size; just the
		// size of the requested range.
		size = c.HTTPResponse.ContentLength
	}

	if size == 0 {
		return false, nil
	}

	if size < c.fi.Size() {
		return false, ErrBadLength
	}

	if size == c.fi.Size() {
		c.DidResume = true
		c.bytesResumed = c.fi.Size()
		c.bytesTransferred = c.fi.Size()
		if err := c.checksum(); err != nil {
			return false, err
		}

		return true, nil
	}

	// prepare for resuming a partial completed download
	if c.CanResume {
		c.Request.HTTPRequest.Header.Set("Range", fmt.Sprintf("bytes=%d-", c.fi.Size()))
		c.DidResume = true
		c.bytesResumed = c.fi.Size()
		c.bytesTransferred = c.fi.Size()
		c.writeFlags = os.O_APPEND | os.O_WRONLY
	}

	return false, nil
}

// openWriter opens the destination file for writing and seeks to the location
// from whence the file transfer will resume.
//
// Requires that Response.Filename and Response.writeFlags already be set.
func (c *Response) openWriter() error {
	if c.Filename == "" {
		panic("filename not set")
	}

	if c.writeFlags == 0 {
		panic("writeFlags not set")
	}

	f, err := os.OpenFile(c.Filename, c.writeFlags, 0644)
	if err != nil {
		return err
	}
	c.writer = f

	// seek to start or end
	whence := os.SEEK_SET
	if c.bytesResumed > 0 {
		whence = os.SEEK_END
	}

	if _, err := f.Seek(0, whence); err != nil {
		return err
	}

	return nil
}

// copy transfers content for a HTTP connection established via Client.do()
func (c *Response) copy() error {
	if c.IsComplete() {
		return c.err
	}

	if c.bufferSize < 1 {
		c.bufferSize = 32 * 1024
	}

	buffer := make([]byte, c.bufferSize)
	for {
		// check for cancellation
		select {
		case <-c.ctx.Done():
			return c.close(c.ctx.Err())

		default:
			// continue
		}

		n, err := c.HTTPResponse.Body.Read(buffer)
		if err != nil && err != io.EOF {
			return c.close(err)
		}

		// TODO: fix buffer underwrites
		if _, werr := c.writer.Write(buffer[:n]); werr != nil {
			return c.close(werr)
		}
		atomic.AddInt64(&c.bytesTransferred, int64(n))

		if err == io.EOF {
			c.HTTPResponse.Body.Close()
			c.writer.Close()
			break
		}
	}

	if err := c.checksum(); err != nil {
		return c.close(err)
	}

	return c.close(nil)
}

// checksum validates a completed file transfer.
func (c *Response) checksum() error {
	if c.Request.hash == nil {
		return nil
	}

	// open downloaded file
	f, err := os.Open(c.Filename)
	if err != nil {
		return err
	}
	defer f.Close()

	// hash file
	if _, err := io.Copy(c.Request.hash, f); err != nil {
		return err
	}

	// compare checksum
	sum := c.Request.hash.Sum(nil)
	if !bytes.Equal(sum, c.Request.checksum) {
		if c.Request.deleteOnError {
			os.Remove(c.Filename)
		}

		return ErrBadChecksum
	}

	return nil
}

// close finalizes the Response
func (c *Response) close(err error) error {
	if c.IsComplete() {
		panic("response already closed")
	}

	if c.writer != nil {
		c.writer.Close()
		c.writer = nil
	}

	if c.HTTPResponse != nil && c.HTTPResponse.Body != nil {
		c.HTTPResponse.Body.Close()
	}

	c.err = err
	c.End = time.Now()
	close(c.Done)

	// may cause re-entry?
	if c.cancel != nil {
		c.cancel()
	}

	return err
}
