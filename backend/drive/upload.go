// Upload for drive
//
// Docs
// Resumable upload: https://developers.google.com/drive/web/manage-uploads#resumable
// Best practices: https://developers.google.com/drive/web/manage-uploads#best-practices
// Files insert: https://developers.google.com/drive/v2/reference/files/insert
// Files update: https://developers.google.com/drive/v2/reference/files/update
//
// This contains code adapted from google.golang.org/api (C) the GO AUTHORS

package drive

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/fs/fserrors"
	"github.com/ncw/rclone/lib/readers"
	"github.com/pkg/errors"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

const (
	// statusResumeIncomplete is the code returned by the Google uploader when the transfer is not yet complete.
	statusResumeIncomplete = 308
)

// resumableUpload is used by the generated APIs to provide resumable uploads.
// It is not used by developers directly.
type resumableUpload struct {
	f      *Fs
	remote string
	// URI is the resumable resource destination provided by the server after specifying "&uploadType=resumable".
	URI string
	// Media is the object being uploaded.
	Media io.Reader
	// MediaType defines the media type, e.g. "image/jpeg".
	MediaType string
	// ContentLength is the full size of the object being uploaded.
	ContentLength int64
	// Return value
	ret *drive.File
}

// Upload the io.Reader in of size bytes with contentType and info
func (f *Fs) Upload(in io.Reader, size int64, contentType string, fileID string, info *drive.File, remote string) (*drive.File, error) {
	params := make(url.Values)
	params.Set("alt", "json")
	params.Set("uploadType", "resumable")
	params.Set("fields", partialFields)
	if f.isTeamDrive {
		params.Set("supportsTeamDrives", "true")
	}
	if *driveKeepRevisionForever {
		params.Set("keepRevisionForever", "true")
	}
	urls := "https://www.googleapis.com/upload/drive/v3/files"
	method := "POST"
	if fileID != "" {
		params.Set("setModifiedDate", "true")
		urls += "/{fileId}"
		method = "PATCH"
	}
	urls += "?" + params.Encode()
	var res *http.Response
	var err error
	err = f.pacer.Call(func() (bool, error) {
		var body io.Reader
		body, err = googleapi.WithoutDataWrapper.JSONReader(info)
		if err != nil {
			return false, err
		}
		var req *http.Request
		req, err = http.NewRequest(method, urls, body)
		if err != nil {
			return false, err
		}
		googleapi.Expand(req.URL, map[string]string{
			"fileId": fileID,
		})
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req.Header.Set("X-Upload-Content-Type", contentType)
		req.Header.Set("X-Upload-Content-Length", fmt.Sprintf("%v", size))
		res, err = f.client.Do(req)
		if err == nil {
			defer googleapi.CloseBody(res)
			err = googleapi.CheckResponse(res)
		}
		return shouldRetry(err)
	})
	if err != nil {
		return nil, err
	}
	loc := res.Header.Get("Location")
	rx := &resumableUpload{
		f:             f,
		remote:        remote,
		URI:           loc,
		Media:         in,
		MediaType:     contentType,
		ContentLength: size,
	}
	return rx.Upload()
}

// Make an http.Request for the range passed in
func (rx *resumableUpload) makeRequest(start int64, body io.ReadSeeker, reqSize int64) *http.Request {
	req, _ := http.NewRequest("POST", rx.URI, body)
	req.ContentLength = reqSize
	if reqSize != 0 {
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %v-%v/%v", start, start+reqSize-1, rx.ContentLength))
	} else {
		req.Header.Set("Content-Range", fmt.Sprintf("bytes */%v", rx.ContentLength))
	}
	req.Header.Set("Content-Type", rx.MediaType)
	return req
}

// rangeRE matches the transfer status response from the server. $1 is
// the last byte index uploaded.
var rangeRE = regexp.MustCompile(`^0\-(\d+)$`)

// Query drive for the amount transferred so far
//
// If error is nil, then start should be valid
func (rx *resumableUpload) transferStatus() (start int64, err error) {
	req := rx.makeRequest(0, nil, 0)
	res, err := rx.f.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer googleapi.CloseBody(res)
	if res.StatusCode == http.StatusCreated || res.StatusCode == http.StatusOK {
		return rx.ContentLength, nil
	}
	if res.StatusCode != statusResumeIncomplete {
		err = googleapi.CheckResponse(res)
		if err != nil {
			return 0, err
		}
		return 0, errors.Errorf("unexpected http return code %v", res.StatusCode)
	}
	Range := res.Header.Get("Range")
	if m := rangeRE.FindStringSubmatch(Range); len(m) == 2 {
		start, err = strconv.ParseInt(m[1], 10, 64)
		if err == nil {
			return start, nil
		}
	}
	return 0, errors.Errorf("unable to parse range %q", Range)
}

// Transfer a chunk - caller must call googleapi.CloseBody(res) if err == nil || res != nil
func (rx *resumableUpload) transferChunk(start int64, chunk io.ReadSeeker, chunkSize int64) (int, error) {
	_, _ = chunk.Seek(0, io.SeekStart)
	req := rx.makeRequest(start, chunk, chunkSize)
	res, err := rx.f.client.Do(req)
	if err != nil {
		return 599, err
	}
	defer googleapi.CloseBody(res)
	if res.StatusCode == statusResumeIncomplete {
		return res.StatusCode, nil
	}
	err = googleapi.CheckResponse(res)
	if err != nil {
		return res.StatusCode, err
	}

	// When the entire file upload is complete, the server
	// responds with an HTTP 201 Created along with any metadata
	// associated with this resource. If this request had been
	// updating an existing entity rather than creating a new one,
	// the HTTP response code for a completed upload would have
	// been 200 OK.
	//
	// So parse the response out of the body.  We aren't expecting
	// any other 2xx codes, so we parse it unconditionaly on
	// StatusCode
	if err = json.NewDecoder(res.Body).Decode(&rx.ret); err != nil {
		return 598, err
	}

	return res.StatusCode, nil
}

// Upload uploads the chunks from the input
// It retries each chunk using the pacer and --low-level-retries
func (rx *resumableUpload) Upload() (*drive.File, error) {
	start := int64(0)
	var StatusCode int
	var err error
	buf := make([]byte, int(chunkSize))
	for start < rx.ContentLength {
		reqSize := rx.ContentLength - start
		if reqSize >= int64(chunkSize) {
			reqSize = int64(chunkSize)
		}
		chunk := readers.NewRepeatableLimitReaderBuffer(rx.Media, buf, reqSize)

		// Transfer the chunk
		err = rx.f.pacer.Call(func() (bool, error) {
			fs.Debugf(rx.remote, "Sending chunk %d length %d", start, reqSize)
			StatusCode, err = rx.transferChunk(start, chunk, reqSize)
			again, err := shouldRetry(err)
			if StatusCode == statusResumeIncomplete || StatusCode == http.StatusCreated || StatusCode == http.StatusOK {
				again = false
				err = nil
			}
			return again, err
		})
		if err != nil {
			return nil, err
		}

		start += reqSize
	}
	// Resume or retry uploads that fail due to connection interruptions or
	// any 5xx errors, including:
	//
	// 500 Internal Server Error
	// 502 Bad Gateway
	// 503 Service Unavailable
	// 504 Gateway Timeout
	//
	// Use an exponential backoff strategy if any 5xx server error is
	// returned when resuming or retrying upload requests. These errors can
	// occur if a server is getting overloaded. Exponential backoff can help
	// alleviate these kinds of problems during periods of high volume of
	// requests or heavy network traffic.  Other kinds of requests should not
	// be handled by exponential backoff but you can still retry a number of
	// them. When retrying these requests, limit the number of times you
	// retry them. For example your code could limit to ten retries or less
	// before reporting an error.
	//
	// Handle 404 Not Found errors when doing resumable uploads by starting
	// the entire upload over from the beginning.
	if rx.ret == nil {
		return nil, fserrors.RetryErrorf("Incomplete upload - retry, last error %d", StatusCode)
	}
	return rx.ret, nil
}
