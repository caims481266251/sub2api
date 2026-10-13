package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"
)

// Only the borrowed-route verifier opts in. Business streams and account probes
// keep their own lifecycle and never inherit early rejection from this reader.
type borrowProbeResponseKey struct{}

// A rejected candidate cannot become usable by waiting for more output. Allow
// a bounded first-event read to retain immediately returned structured health
// errors, then close the stream. This is not a configurable validation timeout.
const borrowProbeRejectionGrace = 250 * time.Millisecond

func readBorrowProbeResponse(body io.ReadCloser, rejected bool, cancel context.CancelFunc) ([]byte, error) {
	var timer *time.Timer
	if rejected {
		timer = time.AfterFunc(borrowProbeRejectionGrace, cancel)
		defer timer.Stop()
	}
	status := &borrowProbeReadStatus{Reader: body}
	reader := bufio.NewReader(io.LimitReader(status, openAICodexStateProbeMaxBody+1))
	var data, payload []byte
	for {
		line, err := reader.ReadBytes('\n')
		data = append(data, line...)
		if len(data) > openAICodexStateProbeMaxBody {
			return data, nil
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			if len(payload) > 0 {
				payload = append(payload, '\n')
			}
			payload = append(payload, bytes.TrimPrefix(trimmed, []byte("data:"))...)
		}
		if len(trimmed) == 0 && len(line) > 0 {
			var event struct {
				Type string `json:"type"`
			}
			valid := json.Unmarshal(payload, &event) == nil && event.Type != ""
			payload = nil
			if valid && (rejected || event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "response.incomplete" || event.Type == "error") {
				// Preserve health signals already in the same buffered network read.
				if rejected && reader.Buffered() > 0 {
					buffered, _ := reader.Peek(reader.Buffered())
					data = append(data, buffered...)
				}
				// bufio may have buffered terminal bytes together with a deadline
				// error. Never turn a late completion into a successful probe.
				if status.err != nil && status.err != io.EOF {
					return data, status.err
				}
				return data, nil
			}
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return data, err
		}
	}
}

// Preserve errors returned with bytes; bufio defers them until its next read.
type borrowProbeReadStatus struct {
	io.Reader
	err error
}

func (r *borrowProbeReadStatus) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil {
		r.err = err
	}
	return n, err
}
